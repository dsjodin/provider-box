package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
)

// netboxAPI is a minimal client for the NetBox REST API over the pinned
// HTTPS endpoint (fqdn resolved to 127.0.0.1, step-ca root trusted).
// NetBox 4.6 uses hashed v2 tokens sent as "Authorization: Bearer
// nbt_<key>.<token>"; the legacy "Token <key>" header only applies to pre-4.6
// responses that carry no "token" field.
type netboxAPI struct {
	base   string // https://<NETBOX_FQDN>:<NETBOX_PORT>
	auth   string // full Authorization header value
	client *http.Client
}

func newNetboxAPI(env map[string]string) (*netboxAPI, error) {
	client, err := pinnedHTTPSClient(filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt"))
	if err != nil {
		return nil, err
	}
	return &netboxAPI{
		base:   fmt.Sprintf("https://%s:%s", env["NETBOX_FQDN"], env["NETBOX_PORT"]),
		client: client,
	}, nil
}

// request performs method on endpoint with an optional JSON payload and
// decodes the response body into out (nil to discard). Non-2xx/3xx fails.
func (n *netboxAPI) request(ctx context.Context, method, endpoint string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.base+endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if n.auth != "" {
		req.Header.Set("Authorization", n.auth)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return fmt.Errorf("NetBox API %s %s returned HTTP %d: %.300s", method, endpoint, resp.StatusCode, respBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("NetBox API %s %s: bad JSON: %w", method, endpoint, err)
		}
	}
	return nil
}

// statusWith probes endpoint with an explicit Authorization header value and
// returns the HTTP status (0 on transport error).
func (n *netboxAPI) statusWith(ctx context.Context, authHeader, endpoint string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base+endpoint, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", authHeader)
	resp, err := n.client.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

type netboxObject struct {
	ID int `json:"id"`
}

type netboxList struct {
	Results []netboxObject `json:"results"`
}

// getObjectID returns the first matching object's id, or 0.
func (n *netboxAPI) getObjectID(ctx context.Context, endpoint, query string) (int, error) {
	var list netboxList
	if err := n.request(ctx, http.MethodGet, endpoint+"?"+query+"&brief=1", nil, &list); err != nil {
		return 0, err
	}
	if len(list.Results) == 0 {
		return 0, nil
	}
	return list.Results[0].ID, nil
}

// createObject POSTs payload and returns the new object's id.
func (n *netboxAPI) createObject(ctx context.Context, endpoint string, payload any) (int, error) {
	var obj netboxObject
	if err := n.request(ctx, http.MethodPost, endpoint, payload, &obj); err != nil {
		return 0, err
	}
	if obj.ID == 0 {
		return 0, fmt.Errorf("NetBox create on %s returned no id", endpoint)
	}
	return obj.ID, nil
}

func (n *netboxAPI) patchObject(ctx context.Context, endpoint string, id int, payload any) error {
	return n.request(ctx, http.MethodPatch, fmt.Sprintf("%s%d/", endpoint, id), payload, nil)
}

// ensureObject looks an object up by query and creates it when absent,
// returning the id.
func (n *netboxAPI) ensureObject(ctx context.Context, endpoint, query string, payload any) (int, error) {
	id, err := n.getObjectID(ctx, endpoint, query)
	if err != nil || id != 0 {
		return id, err
	}
	return n.createObject(ctx, endpoint, payload)
}

type netboxTokenProvision struct {
	ID    int    `json:"id"`
	Key   string `json:"key"`
	Token string `json:"token"`
}

// provisionToken creates an API token for username/password. The returned
// header value is "Bearer nbt_<key>.<token>" for v2 responses or the legacy
// "Token <key>" when no token field is present.
func (n *netboxAPI) provisionToken(ctx context.Context, username, password, description string, writeEnabled bool) (header string, composite string, id int, err error) {
	payload := map[string]any{"username": username, "password": password}
	if description != "" {
		payload["description"] = description
	}
	if !writeEnabled {
		payload["write_enabled"] = false
	}
	var out netboxTokenProvision
	if err := n.request(ctx, http.MethodPost, "/api/users/tokens/provision/", payload, &out); err != nil {
		return "", "", 0, err
	}
	if out.Key == "" {
		return "", "", 0, fmt.Errorf("NetBox token provision returned no key")
	}
	if out.Token != "" {
		composite = "nbt_" + out.Key + "." + out.Token
		return "Bearer " + composite, composite, out.ID, nil
	}
	return "Token " + out.Key, out.Key, out.ID, nil
}

// retireTokensByDescription deletes previous provider-box tokens matching a
// description so redeploys do not accumulate live credentials. Best-effort.
func (n *netboxAPI) retireTokensByDescription(ctx context.Context, rc *RunCtx, description, label string) {
	var list netboxList
	q := "/api/users/tokens/?description=" + url.QueryEscape(description)
	if err := n.request(ctx, http.MethodGet, q, nil, &list); err != nil {
		rc.Log("NOTICE: could not enumerate previous %s NetBox tokens: %v", label, err)
		return
	}
	for _, tok := range list.Results {
		if err := n.request(ctx, http.MethodDelete, fmt.Sprintf("/api/users/tokens/%d/", tok.ID), nil, nil); err != nil {
			rc.Log("NOTICE: could not delete previous %s NetBox token id %d: %v", label, tok.ID, err)
		} else {
			rc.Log("Deleted previous %s NetBox token (id %d).", label, tok.ID)
		}
	}
}

