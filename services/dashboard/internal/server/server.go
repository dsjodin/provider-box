// Package server collects current state from each upstream and renders it as an
// HTML page or JSON. Every panel is fetched under its own timeout and its
// errors are isolated: a dead or unconfigured source renders as "unavailable"
// or "not configured" and never blanks the page or fails the request.
package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dsjodin/provider-box/services/dashboard/internal/certs"
	"github.com/dsjodin/provider-box/services/dashboard/internal/dns"
	"github.com/dsjodin/provider-box/services/dashboard/internal/docker"
	"github.com/dsjodin/provider-box/services/dashboard/internal/ipam"
	"github.com/dsjodin/provider-box/services/dashboard/internal/logs"
)

// Provider interfaces let the server run against real clients or test stubs.
type (
	CertProvider interface {
		List(ctx context.Context) ([]certs.Cert, error)
	}
	DNSProvider interface {
		Fetch(ctx context.Context) (dns.Overview, error)
	}
	IPAMProvider interface {
		Fetch(ctx context.Context) (ipam.Overview, error)
	}
	DockerProvider interface {
		List(ctx context.Context, nameFilters []string, now time.Time) ([]docker.Container, error)
		LogLines(ctx context.Context, id string, tail int) ([]string, error)
	}
)

type Options struct {
	FQDN             string
	WarnDays         int
	LogTail          int
	ContainerFilters []string
	Timeout          time.Duration
	MaxErrorLines    int

	Certs  CertProvider
	DNS    DNSProvider
	IPAM   IPAMProvider
	Docker DockerProvider

	Logger *slog.Logger
	Now    func() time.Time // injectable clock; defaults to time.Now
}

type Server struct {
	opt  Options
	tmpl *template.Template
}

//go:embed templates/dashboard.html
var dashboardHTML string

func New(opt Options) (*Server, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 5 * time.Second
	}
	if opt.MaxErrorLines <= 0 {
		opt.MaxErrorLines = 50
	}
	tmpl, err := template.New("dashboard").Funcs(tmplFuncs).Parse(dashboardHTML)
	if err != nil {
		return nil, err
	}
	return &Server{opt: opt, tmpl: tmpl}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page := s.collect(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, page); err != nil {
		s.opt.Logger.Error("render dashboard", "err", err)
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	page := s.collect(r.Context())
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(page)
}

// collect fetches every panel concurrently, each under its own timeout, and
// returns the assembled page. A panic or error in one fetch cannot affect
// another.
func (s *Server) collect(ctx context.Context) Page {
	now := s.opt.Now()
	page := Page{
		FQDN:        s.opt.FQDN,
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}

	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		page.Certs = s.collectCerts(ctx, now)
	}()
	go func() {
		defer wg.Done()
		page.DNS = s.collectDNS(ctx)
	}()
	go func() {
		defer wg.Done()
		page.IPAM = s.collectIPAM(ctx)
	}()
	go func() {
		defer wg.Done()
		page.Services, page.Errors = s.collectDocker(ctx, now)
	}()

	wg.Wait()
	return page
}

func (s *Server) panelCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.opt.Timeout)
}

func (s *Server) collectCerts(ctx context.Context, now time.Time) CertsPanel {
	p := CertsPanel{}
	if s.opt.Certs == nil {
		p.Status = disabled("DASHBOARD_STEPCA_DB not set")
		return p
	}
	pctx, cancel := s.panelCtx(ctx)
	defer cancel()
	raw, err := s.opt.Certs.List(pctx)
	if err != nil {
		p.Status = unavailable(err)
		return p
	}
	p.Summary = certs.Summarize(raw, now, s.opt.WarnDays)
	p.Status = ok()
	return p
}

func (s *Server) collectDNS(ctx context.Context) DNSPanel {
	p := DNSPanel{}
	if s.opt.DNS == nil {
		p.Status = disabled("DASHBOARD_TECHNITIUM_URL/token not set")
		return p
	}
	pctx, cancel := s.panelCtx(ctx)
	defer cancel()
	ov, err := s.opt.DNS.Fetch(pctx)
	if err != nil {
		p.Status = unavailable(err)
		return p
	}
	p.Overview = ov
	p.Status = ok()
	return p
}

func (s *Server) collectIPAM(ctx context.Context) IPAMPanel {
	p := IPAMPanel{}
	if s.opt.IPAM == nil {
		p.Status = disabled("DASHBOARD_NETBOX_URL/token not set")
		return p
	}
	pctx, cancel := s.panelCtx(ctx)
	defer cancel()
	ov, err := s.opt.IPAM.Fetch(pctx)
	if err != nil {
		p.Status = unavailable(err)
		return p
	}
	p.Overview = ov
	p.Status = ok()
	return p
}

// collectDocker builds both the services panel and the recent-errors panel from
// one container listing so a single Docker failure degrades both together.
func (s *Server) collectDocker(ctx context.Context, now time.Time) (ServicesPanel, ErrorsPanel) {
	svc := ServicesPanel{}
	errp := ErrorsPanel{}
	if s.opt.Docker == nil {
		svc.Status = disabled("DASHBOARD_DOCKER_HOST not available")
		errp.Status = disabled("DASHBOARD_DOCKER_HOST not available")
		return svc, errp
	}

	pctx, cancel := s.panelCtx(ctx)
	defer cancel()
	containers, err := s.opt.Docker.List(pctx, s.opt.ContainerFilters, now)
	if err != nil {
		svc.Status = unavailable(err)
		errp.Status = unavailable(err)
		return svc, errp
	}
	svc.Containers = containers
	svc.Status = ok()

	// Errors panel: tail each running container's log under its own short
	// budget and stop once MaxErrorLines is reached.
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		if len(errp.Entries) >= s.opt.MaxErrorLines {
			break
		}
		lctx, lcancel := context.WithTimeout(ctx, s.opt.Timeout)
		lines, lerr := s.opt.Docker.LogLines(lctx, c.ID, s.opt.LogTail)
		lcancel()
		if lerr != nil {
			s.opt.Logger.Warn("tail logs", "container", c.Name, "err", lerr)
			continue
		}
		for _, e := range logs.Extract(c.Name, lines) {
			errp.Entries = append(errp.Entries, e)
			if len(errp.Entries) >= s.opt.MaxErrorLines {
				break
			}
		}
	}
	errp.Status = ok()
	return svc, errp
}
