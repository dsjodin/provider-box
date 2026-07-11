// Package docker reads container status and logs from the Docker Engine API
// over a socket mounted read-only. Only GET endpoints are used
// (containers/json, containers/{id}/logs); nothing is created, started, or
// stopped.
package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client talks to the Docker Engine API. It supports a unix socket
// (unix:///var/run/docker.sock, the default) or a tcp:// host.
type Client struct {
	HTTP    *http.Client
	baseURL string
}

// New builds a Client for the given Docker host. "" defaults to the standard
// unix socket.
func New(host string, timeout time.Duration) (*Client, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	scheme, addr, ok := strings.Cut(host, "://")
	if !ok {
		return nil, fmt.Errorf("invalid docker host %q: want scheme://addr", host)
	}
	tr := &http.Transport{}
	base := ""
	switch scheme {
	case "unix":
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", addr)
		}
		base = "http://unix"
	case "tcp", "http":
		base = "http://" + addr
	default:
		return nil, fmt.Errorf("unsupported docker host scheme %q", scheme)
	}
	return &Client{
		HTTP:    &http.Client{Transport: tr, Timeout: timeout},
		baseURL: base,
	}, nil
}

// Container is one container's current state.
type Container struct {
	ID      string
	Name    string
	Image   string
	State   string // running, exited, created, ...
	Status  string // e.g. "Up 2 hours (healthy)"
	Health  string // healthy, unhealthy, starting, or "" when none
	Uptime  string // human uptime derived from Created for running containers
	Project string // com.docker.compose.project label
	Created time.Time
}

type containerJSON struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

// List returns all containers whose name contains any of nameFilters (empty
// nameFilters returns every container). now is used to derive uptime.
func (c *Client) List(ctx context.Context, nameFilters []string, now time.Time) ([]Container, error) {
	var raw []containerJSON
	if err := c.getJSON(ctx, "/containers/json?all=1", &raw); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		name := ""
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		if !matchesAny(name, nameFilters) {
			continue
		}
		created := time.Unix(r.Created, 0)
		ct := Container{
			ID:      r.ID,
			Name:    name,
			Image:   r.Image,
			State:   r.State,
			Status:  r.Status,
			Health:  parseHealth(r.Status),
			Project: r.Labels["com.docker.compose.project"],
			Created: created,
		}
		if r.State == "running" {
			ct.Uptime = humanDuration(now.Sub(created))
		}
		out = append(out, ct)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LogLines returns up to tail demultiplexed log lines for a container,
// newest-last. stderr and stdout are merged.
func (c *Client) LogLines(ctx context.Context, id string, tail int) ([]string, error) {
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("tail", strconv.Itoa(tail))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/containers/"+id+"/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker logs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("docker logs %s: status %d body=%s", id, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return splitLogLines(demux(data)), nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("docker GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("docker GET %s: status %d body=%s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func matchesAny(name string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if f != "" && strings.Contains(name, f) {
			return true
		}
	}
	return false
}

// parseHealth extracts the health word from a Docker status string such as
// "Up 2 hours (healthy)" or "Up 5 seconds (health: starting)".
func parseHealth(status string) string {
	open := strings.LastIndex(status, "(")
	close := strings.LastIndex(status, ")")
	if open < 0 || close < open {
		return ""
	}
	inner := strings.TrimSpace(status[open+1 : close])
	inner = strings.TrimPrefix(inner, "health: ")
	switch inner {
	case "healthy", "unhealthy", "starting":
		return inner
	default:
		return ""
	}
}

// demux converts a Docker multiplexed log stream into plain bytes. A stream
// without a TTY is framed as [type(1)][0 0 0][size(4 BE)][payload]; a TTY
// stream is raw. When the leading bytes do not look like a valid frame header
// the input is returned unchanged (raw stream).
func demux(b []byte) []byte {
	if !looksFramed(b) {
		return b
	}
	var out []byte
	for len(b) >= 8 {
		size := binary.BigEndian.Uint32(b[4:8])
		b = b[8:]
		if uint32(len(b)) < size {
			out = append(out, b...)
			break
		}
		out = append(out, b[:size]...)
		b = b[size:]
	}
	return out
}

func looksFramed(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	// Frame type is 0, 1, or 2; bytes 1-3 are always zero padding.
	return b[0] <= 2 && b[1] == 0 && b[2] == 0 && b[3] == 0
}

func splitLogLines(b []byte) []string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}
