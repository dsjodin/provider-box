// Package ipam reads current state from NetBox using the same IPAM API shapes
// the dns-sync netbox client already uses. Read-only: only GET on
// ipam/prefixes and ipam/ip-addresses. Use a dedicated, minimum-read-scope
// token here - never the dns-sync or bootstrap admin token.
package ipam

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// Client is a read-only NetBox IPAM client.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New builds a Client. caBundle may be "" to use the system trust store.
func New(baseURL, token, caBundle string, timeout time.Duration) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("netbox base url is required")
	}
	if token == "" {
		return nil, errors.New("netbox token is required")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if caBundle != "" {
		pem, err := os.ReadFile(caBundle)
		if err != nil {
			return nil, fmt.Errorf("read netbox CA bundle %s: %w", caBundle, err)
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

const pageSize = 500

// Overview is the shaped IPAM panel.
type Overview struct {
	PrefixCount int
	IPCount     int
	DNSNames    []string // sorted unique dns_name values (blank names excluded)
}

// authHeader mirrors dns-sync: NetBox 4.6 v2 tokens are the composite
// "nbt_<key>.<token>" sent as Bearer; anything else is a legacy v1 Token.
func (c *Client) authHeader() string {
	if strings.HasPrefix(c.Token, "nbt_") {
		return "Bearer " + c.Token
	}
	return "Token " + c.Token
}

type listResp struct {
	Count   int             `json:"count"`
	Next    *string         `json:"next"`
	Results json.RawMessage `json:"results"`
}

type ipResult struct {
	DNSName string `json:"dns_name"`
}

// Fetch returns prefix/IP counts and the sorted unique dns_name inventory.
func (c *Client) Fetch(ctx context.Context) (Overview, error) {
	var ov Overview

	prefixCount, _, err := c.getPage(ctx, fmt.Sprintf("%s/api/ipam/prefixes/?limit=1", c.BaseURL))
	if err != nil {
		return ov, fmt.Errorf("count prefixes: %w", err)
	}
	ov.PrefixCount = prefixCount

	names := map[string]struct{}{}
	next := fmt.Sprintf("%s/api/ipam/ip-addresses/?limit=%d", c.BaseURL, pageSize)
	for next != "" {
		count, page, err := c.getPage(ctx, next)
		if err != nil {
			return ov, fmt.Errorf("list ip-addresses: %w", err)
		}
		ov.IPCount = count
		var results []ipResult
		if err := json.Unmarshal(page.Results, &results); err != nil {
			return ov, fmt.Errorf("decode ip-addresses: %w", err)
		}
		for _, r := range results {
			if n := strings.TrimSpace(r.DNSName); n != "" {
				names[n] = struct{}{}
			}
		}
		if page.Next != nil {
			next = *page.Next
		} else {
			next = ""
		}
	}

	ov.DNSNames = make([]string, 0, len(names))
	for n := range names {
		ov.DNSNames = append(ov.DNSNames, n)
	}
	sort.Strings(ov.DNSNames)
	return ov, nil
}

func (c *Client) getPage(ctx context.Context, url string) (int, *listResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("netbox GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return 0, nil, fmt.Errorf("netbox GET %s: status %d body=%s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out listResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, nil, fmt.Errorf("decode netbox response: %w", err)
	}
	return out.Count, &out, nil
}
