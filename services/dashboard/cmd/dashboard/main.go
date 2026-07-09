package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dsjodin/provider-box/services/dashboard/internal/certs"
	"github.com/dsjodin/provider-box/services/dashboard/internal/config"
	"github.com/dsjodin/provider-box/services/dashboard/internal/dns"
	"github.com/dsjodin/provider-box/services/dashboard/internal/docker"
	"github.com/dsjodin/provider-box/services/dashboard/internal/ipam"
	"github.com/dsjodin/provider-box/services/dashboard/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg := config.Load()

	opt := server.Options{
		FQDN:             cfg.FQDN,
		WarnDays:         cfg.CertWarnDays,
		LogTail:          cfg.LogTail,
		ContainerFilters: cfg.ContainerFilters,
		Timeout:          cfg.UpstreamTimeout,
		Logger:           logger,
	}

	// Each provider is optional; a nil provider renders its panel as
	// "not configured" rather than failing the page.
	if cfg.StepCADBPath != "" {
		opt.Certs = &certs.Reader{Path: cfg.StepCADBPath, SnapshotRoot: cfg.StepCASnapshot}
	}
	if cfg.TechnitiumURL != "" && cfg.TechnitiumToken != "" {
		c, err := dns.New(cfg.TechnitiumURL, cfg.TechnitiumToken, cfg.TechnitiumCABundle, cfg.UpstreamTimeout)
		if err != nil {
			logger.Error("init technitium client", "err", err)
		} else {
			opt.DNS = c
		}
	}
	if cfg.NetboxURL != "" && cfg.NetboxToken != "" {
		c, err := ipam.New(cfg.NetboxURL, cfg.NetboxToken, cfg.NetboxCABundle, cfg.UpstreamTimeout)
		if err != nil {
			logger.Error("init netbox client", "err", err)
		} else {
			opt.IPAM = c
		}
	}
	if cfg.DockerHost != "" {
		c, err := docker.New(cfg.DockerHost, cfg.UpstreamTimeout)
		if err != nil {
			logger.Error("init docker client", "err", err)
		} else {
			opt.Docker = c
		}
	}

	srv, err := server.New(opt)
	if err != nil {
		logger.Error("build server", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	tls := cfg.TLSCert != "" && cfg.TLSKey != ""
	logger.Info("starting dashboard",
		"addr", cfg.Addr, "fqdn", cfg.FQDN, "tls", tls,
		"certs", opt.Certs != nil, "dns", opt.DNS != nil,
		"ipam", opt.IPAM != nil, "docker", opt.Docker != nil,
	)

	if tls {
		err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		logger.Warn("no TLS cert configured (DASHBOARD_TLS_CERT/DASHBOARD_TLS_KEY); serving plaintext HTTP - do not use outside a trusted lab")
		err = httpSrv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server exited", "err", err)
		os.Exit(1)
	}
}
