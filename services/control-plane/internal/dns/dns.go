// Package dns reads current state from Technitium DNS Server using the same
// API shapes the dns-sync client and technitium bootstrap module already use
// (services/dns-sync/TECHNITIUM_API.md). Read-only: only zones/list,
// zones/records/get, and settings/get are called - no create/delete/set.
package dns

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Client is a read-only Technitium client.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New builds a Client. baseURL is the Technitium console root (the HTTPS
// console/API URL, e.g. https://dns.sddc.lab:53443). caBundle is an optional
// PEM bundle path; "" uses the system trust store.
func New(baseURL, token, caBundle string, timeout time.Duration) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("technitium base url is required")
	}
	if token == "" {
		return nil, errors.New("technitium token is required")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if caBundle != "" {
		pem, err := os.ReadFile(caBundle)
		if err != nil {
			return nil, fmt.Errorf("read technitium CA bundle %s: %w", caBundle, err)
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates parsed from %s", caBundle)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Transport: tr, Timeout: timeout},
	}, nil
}

// Overview is the shaped DNS panel.
type Overview struct {
	Zones      []ZoneInfo
	Forwarders []string
	TLSPort    int  // webServiceTlsPort from settings
	TLSEnabled bool // webServiceEnableTls from settings
	// TLSReachable is true because this client reached Technitium over its
	// HTTPS console URL; it is set by the caller after a successful call.
	TLSReachable bool
}

// ZoneInfo is one non-internal zone with its managed record count.
type ZoneInfo struct {
	Name        string
	Type        string
	RecordCount int
}

// envelope is the common Technitium response wrapper; the server always returns
// HTTP 200 and the real status is in the body.
type envelope struct {
	Status       string          `json:"status"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
}

func (c *Client) call(ctx context.Context, path string, params url.Values, out any) error {
	params.Set("token", c.Token)
	u := c.BaseURL + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("technitium request: %w", err)
	}
	defer resp.Body.Close()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode technitium response: %w", err)
	}
	if env.Status != "ok" {
		return fmt.Errorf("technitium: status=%s message=%q", env.Status, env.ErrorMessage)
	}
	if out != nil && len(env.Response) > 0 {
		if err := json.Unmarshal(env.Response, out); err != nil {
			return fmt.Errorf("decode technitium response body: %w", err)
		}
	}
	return nil
}

type zoneEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Internal bool   `json:"internal"`
}

type zoneListResp struct {
	Zones []zoneEntry `json:"zones"`
}

type recordEntry struct {
	Type string `json:"type"`
}

type recordsResp struct {
	Records []recordEntry `json:"records"`
}

type settingsResp struct {
	Forwarders          []string `json:"forwarders"`
	WebServiceEnableTLS bool     `json:"webServiceEnableTls"`
	WebServiceTLSPort   int      `json:"webServiceTlsPort"`
}

// Fetch returns the current DNS overview. The internal (RFC 6303) zones are
// filtered out, matching dns-sync. Record counts exclude auto-generated
// NS/SOA records so the number reflects managed content.
func (c *Client) Fetch(ctx context.Context) (Overview, error) {
	var ov Overview

	var zl zoneListResp
	if err := c.call(ctx, "/api/zones/list", url.Values{}, &zl); err != nil {
		return ov, fmt.Errorf("list zones: %w", err)
	}
	for _, z := range zl.Zones {
		if z.Internal {
			continue
		}
		params := url.Values{}
		params.Set("zone", z.Name)
		params.Set("domain", z.Name)
		params.Set("listZone", "true")
		var rr recordsResp
		if err := c.call(ctx, "/api/zones/records/get", params, &rr); err != nil {
			return ov, fmt.Errorf("get records for %s: %w", z.Name, err)
		}
		ov.Zones = append(ov.Zones, ZoneInfo{
			Name:        z.Name,
			Type:        z.Type,
			RecordCount: countManaged(rr.Records),
		})
	}
	sort.Slice(ov.Zones, func(i, j int) bool { return ov.Zones[i].Name < ov.Zones[j].Name })

	var st settingsResp
	if err := c.call(ctx, "/api/settings/get", url.Values{}, &st); err != nil {
		return ov, fmt.Errorf("get settings: %w", err)
	}
	ov.Forwarders = st.Forwarders
	ov.TLSEnabled = st.WebServiceEnableTLS
	ov.TLSPort = st.WebServiceTLSPort
	ov.TLSReachable = true // every call above went over the HTTPS console URL
	return ov, nil
}

// countManaged counts records excluding the auto-generated NS/SOA entries that
// Technitium always returns in a zone listing.
func countManaged(records []recordEntry) int {
	n := 0
	for _, r := range records {
		switch r.Type {
		case "NS", "SOA":
			continue
		default:
			n++
		}
	}
	return n
}
