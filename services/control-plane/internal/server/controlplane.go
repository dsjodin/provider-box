package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dsjodin/provider-box/services/control-plane/internal/deploy"
	"github.com/dsjodin/provider-box/services/control-plane/internal/envfile"
)

//go:embed templates/wizard.html
var wizardHTML []byte

//go:embed templates/deploy.html
var deployHTML []byte

const maxConfigBytes = 1 << 20 // an env file is a few KB; reject anything absurd

// registerControlPlane wires the config wizard and deploy engine routes when
// an engine is configured. Without one (the read-only --dashboard deployment)
// the dashboard keeps working and these routes simply do not exist.
func (s *Server) registerControlPlane(mux *http.ServeMux) {
	if s.opt.Engine == nil {
		return
	}
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(wizardHTML)
	})
	mux.HandleFunc("GET /deploy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(deployHTML)
	})
	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("POST /api/config/validate", s.handleConfigValidate)
	mux.HandleFunc("PUT /api/config", s.handleConfigPut)
	mux.HandleFunc("GET /api/seed", s.handleSeedGet)
	mux.HandleFunc("PUT /api/seed", s.handleSeedPut)
	mux.HandleFunc("GET /api/services", s.handleServices)
	mux.HandleFunc("POST /api/deploy", s.handleDeploy)
	mux.HandleFunc("GET /api/deploys/{id}/events", s.handleDeployEvents)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// handleConfigGet serves the managed config, or the shipped example when
// nothing has been uploaded yet, as a downloadable env file.
func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	content, saved, err := s.opt.Engine.Store.Load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Provider-Box-Config-Saved", strconv.FormatBool(saved))
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="provider-box.env"`)
	}
	_, _ = w.Write(content)
}

type validateResponse struct {
	Issues  []envfile.Issue `json:"issues"`
	Missing []string        `json:"missing_vars"`
	Valid   bool            `json:"valid"`
}

func (s *Server) validateBody(r *http.Request) (validateResponse, []byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxConfigBytes+1))
	if err != nil {
		return validateResponse{}, nil, err
	}
	if len(body) > maxConfigBytes {
		return validateResponse{}, nil, fmt.Errorf("config too large")
	}
	resp := validateResponse{Issues: []envfile.Issue{}, Missing: []string{}}
	vars := envfile.Parse(body)
	if issues := envfile.ValidateAll(vars); issues != nil {
		resp.Issues = issues
	}
	if example, err := s.opt.Engine.Store.Example(); err == nil {
		if missing := envfile.MissingFromExample(body, example); missing != nil {
			resp.Missing = missing
		}
	}
	resp.Valid = len(resp.Issues) == 0 && len(resp.Missing) == 0
	return resp, body, nil
}

func (s *Server) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	resp, _, err := s.validateBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConfigPut validates and atomically saves the managed config. Missing
// variables block the save (the deploy engine would reject the file anyway);
// value-level issues are returned but do not block, so an operator can save
// incrementally while filling in secrets.
func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	resp, body, err := s.validateBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(resp.Missing) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, resp)
		return
	}
	if err := s.opt.Engine.Store.Save(body); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// seedPath is the managed dns.seed location next to the managed config; the
// netbox and dns-sync deployers read the same path.
func (s *Server) seedPath() string {
	return filepath.Join(filepath.Dir(s.opt.Engine.Store.Path), "dns.seed")
}

// handleSeedGet serves the managed dns.seed (empty when none is saved).
func (s *Server) handleSeedGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	b, err := os.ReadFile(s.seedPath())
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	_, _ = w.Write(b)
}

// handleSeedPut validates each record line (<fqdn> <ip[/cidr]>) and saves the
// file; an empty body deletes it (dns.seed is optional).
func (s *Server) handleSeedPut(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxConfigBytes+1))
	if err != nil || len(body) > maxConfigBytes {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad or oversized seed file"))
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		_ = os.Remove(s.seedPath())
		writeJSON(w, http.StatusOK, map[string]any{"saved": false, "removed": true})
		return
	}
	if issues := validateSeed(body); len(issues) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"issues": issues})
		return
	}
	if err := os.WriteFile(s.seedPath(), body, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

func validateSeed(content []byte) []string {
	var issues []string
	for i, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			issues = append(issues, fmt.Sprintf("line %d: expected <fqdn> <ip> or <fqdn> <ip/cidr>", i+1))
			continue
		}
		value := fields[1]
		if strings.Contains(value, "/") {
			if _, err := netip.ParsePrefix(value); err != nil {
				issues = append(issues, fmt.Sprintf("line %d: invalid CIDR %q", i+1, value))
			}
		} else if _, err := netip.ParseAddr(value); err != nil {
			issues = append(issues, fmt.Sprintf("line %d: invalid IP %q", i+1, value))
		}
	}
	return issues
}

type serviceInfo struct {
	Name       string   `json:"name"`
	Deps       []string `json:"deps"`
	LastAction string   `json:"last_action,omitempty"`
	LastResult string   `json:"last_result,omitempty"`
	LastAt     string   `json:"last_at,omitempty"`
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	var state deploy.State
	if s.opt.Engine.State != nil {
		state = s.opt.Engine.State.Snapshot()
	}
	var out []serviceInfo
	for _, svc := range s.opt.Engine.Services() {
		info := serviceInfo{Name: svc.Name(), Deps: svc.Deps()}
		if st, ok := state.Services[svc.Name()]; ok {
			info.LastAction = st.LastAction
			info.LastResult = st.Result
			info.LastAt = st.At.Format("2006-01-02 15:04:05 UTC")
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

type deployRequest struct {
	Services []string `json:"services"`
	Remove   bool     `json:"remove"`
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Services) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("no services selected"))
		return
	}
	id, err := s.opt.Engine.Start(req.Services, req.Remove)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, deploy.ErrBusy) {
			status = http.StatusConflict
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"id": id})
}

// handleDeployEvents streams a run's progress as SSE, replaying buffered
// events first so late subscribers get the full log.
func (s *Server) handleDeployEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad deploy id"))
		return
	}
	run := s.opt.Engine.Run(id)
	if run == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such deploy"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	send := func(ev deploy.Event) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	replay, live := run.Subscribe()
	for _, ev := range replay {
		send(ev)
	}
	flusher.Flush()
	if live == nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-live:
			if !ok {
				return
			}
			send(ev)
			flusher.Flush()
		}
	}
}
