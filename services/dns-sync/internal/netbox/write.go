package netbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// EnsurePrefix creates a prefix object for cidr if one does not already exist.
// Returns true if a new prefix was created, false if an existing one was found.
// Idempotent per AGENTS.md NetBox seeding rules.
func (c *Client) EnsurePrefix(ctx context.Context, cidr string) (created bool, err error) {
	q := url.Values{}
	q.Set("prefix", cidr)
	exists, err := c.exists(ctx, "/api/ipam/prefixes/", q)
	if err != nil {
		return false, fmt.Errorf("check prefix %s: %w", cidr, err)
	}
	if exists {
		return false, nil
	}
	body := map[string]any{
		"prefix": cidr,
		"status": "active",
	}
	if err := c.post(ctx, "/api/ipam/prefixes/", body); err != nil {
		return false, fmt.Errorf("create prefix %s: %w", cidr, err)
	}
	return true, nil
}

// EnsureIPAddress creates an ip-address object for addrCIDR with dns_name if
// one does not already exist. AGENTS.md rule: one IP object per address. When
// the same address already has a different dns_name, this is a no-op (the
// existing assignment wins; reconcile is the post-bootstrap edit path).
// Returns true if a new IP was created.
func (c *Client) EnsureIPAddress(ctx context.Context, addrCIDR, dnsName string) (created bool, err error) {
	q := url.Values{}
	q.Set("address", addrCIDR)
	exists, err := c.exists(ctx, "/api/ipam/ip-addresses/", q)
	if err != nil {
		return false, fmt.Errorf("check ip %s: %w", addrCIDR, err)
	}
	if exists {
		return false, nil
	}
	body := map[string]any{
		"address":  addrCIDR,
		"dns_name": dnsName,
		"status":   "active",
	}
	if err := c.post(ctx, "/api/ipam/ip-addresses/", body); err != nil {
		return false, fmt.Errorf("create ip %s: %w", addrCIDR, err)
	}
	return true, nil
}

type countResp struct {
	Count int `json:"count"`
}

func (c *Client) exists(ctx context.Context, path string, q url.Values) (bool, error) {
	q.Set("limit", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Token "+c.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("netbox GET %s: status %d", path, resp.StatusCode)
	}
	var out countResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Count > 0, nil
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("netbox POST %s: status %d body=%s", path, resp.StatusCode, respBody)
	}
	return nil
}
