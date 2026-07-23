package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dsjodin/labprovider/services/control-plane/internal/certs"
	"github.com/dsjodin/labprovider/services/control-plane/internal/config"
	"github.com/dsjodin/labprovider/services/control-plane/internal/deploy"
	"github.com/dsjodin/labprovider/services/control-plane/internal/dns"
	"github.com/dsjodin/labprovider/services/control-plane/internal/docker"
	"github.com/dsjodin/labprovider/services/control-plane/internal/envfile"
	"github.com/dsjodin/labprovider/services/control-plane/internal/ipam"
	"github.com/dsjodin/labprovider/services/control-plane/internal/msca"
	"github.com/dsjodin/labprovider/services/control-plane/internal/server"
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
	if cfg.StepCADSN != "" {
		r, err := certs.NewReader(cfg.StepCADSN, cfg.StepCAPassword)
		if err != nil {
			logger.Error("init stepca certs reader", "err", err)
		} else {
			opt.Certs = r
		}
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

	// Optional Microsoft-CA web-enrollment emulator (certsrv) for VCF; wired
	// below when the managed config enables it.
	var (
		mscaHandler http.Handler
		mscaAddr    string
	)

	// The deploy engine needs the shipped example config (baked into the image
	// by install.sh's build). Without it - the legacy --dashboard deployment -
	// the server stays a read-only dashboard.
	if _, err := os.Stat(cfg.ExamplePath); err == nil {
		store := envfile.Store{Path: cfg.ConfigPath, ExamplePath: cfg.ExamplePath}

		// Engine-enabled deployments resolve the panel upstreams from the
		// managed config at page-load time; explicit CONTROL_PLANE_* env vars
		// (the legacy compose wiring) win when set above.
		src := lazySource{store: store, timeout: cfg.UpstreamTimeout}
		if opt.Certs == nil {
			opt.Certs = lazyCerts{src}
		}
		if opt.DNS == nil {
			opt.DNS = lazyDNS{src}
		}
		if opt.IPAM == nil {
			opt.IPAM = lazyIPAM{src}
		}

		engine := deploy.NewEngine(store, &deploy.StateStore{Path: cfg.StatePath}, logger)
		// Registration order is the --all deploy order: no-dependency services
		// first, then the CA, then certificate consumers as they are ported.
		engine.Register(deploy.Chrony{})
		engine.Register(deploy.Rsyslog{})
		engine.Register(deploy.CA{})
		engine.Register(deploy.Technitium{})
		engine.Register(deploy.Depot{})
		engine.Register(deploy.Keycloak{})
		engine.Register(deploy.Authentik{})
		engine.Register(deploy.Zitadel{})
		engine.Register(deploy.Netbox{})
		engine.Register(deploy.S3{})
		engine.Register(deploy.SFTP{})
		engine.Register(deploy.DNSSync{})
		opt.Engine = engine

		if h, addr, err := buildMSCA(store, logger); err != nil {
			logger.Warn("msca certsrv emulator disabled", "err", err)
		} else if h != nil {
			mscaHandler, mscaAddr = h, addr
		}
	} else {
		logger.Warn("deploy engine disabled: example config not found", "path", cfg.ExamplePath)
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

	// Decide HTTP vs HTTPS before binding. A configured-but-unreadable or
	// malformed cert/key must not crash-loop the server: warn and fall back to
	// HTTP, and reflect the mode actually used in the startup log.
	useTLS := resolveTLS(cfg.TLSCert, cfg.TLSKey, logger)

	// The certsrv emulator is a second listener (its own port) that reuses the
	// control plane's TLS leaf, so VCF reaches it at the control plane FQDN.
	if mscaHandler != nil {
		mscaSrv := &http.Server{Addr: mscaAddr, Handler: mscaHandler, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelShutdown()
			_ = mscaSrv.Shutdown(shutdownCtx)
		}()
		go func() {
			logger.Info("starting msca certsrv emulator", "addr", mscaAddr, "tls", useTLS)
			var err error
			if useTLS {
				err = mscaSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
			} else {
				err = mscaSrv.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("msca listener exited", "err", err)
			}
		}()
	}

	logger.Info("starting control-plane",
		"addr", cfg.Addr, "fqdn", cfg.FQDN, "tls", useTLS,
		"certs", opt.Certs != nil, "dns", opt.DNS != nil,
		"ipam", opt.IPAM != nil, "docker", opt.Docker != nil,
	)

	if useTLS {
		err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server exited", "err", err)
		os.Exit(1)
	}
}

// resolveTLS reports whether the server should serve HTTPS. It returns true only
// when both paths are set and load as a valid keypair; otherwise it logs a
// warning and returns false so the caller serves plaintext HTTP instead of
// crash-looping on a missing or malformed cert.
func resolveTLS(certPath, keyPath string, logger *slog.Logger) bool {
	if certPath == "" || keyPath == "" {
		logger.Warn("no TLS cert configured (CONTROL_PLANE_TLS_CERT/CONTROL_PLANE_TLS_KEY); serving plaintext HTTP - do not use outside a trusted lab")
		return false
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		logger.Warn("TLS cert/key unreadable; falling back to plaintext HTTP - do not use outside a trusted lab",
			"cert", certPath, "key", keyPath, "err", err)
		return false
	}
	return true
}

// buildMSCA constructs the certsrv emulator handler from the managed config
// when VMSCA_ENABLE is true, returning (nil, "", nil) when it is off. The signer
// and CA-chain closures reload the managed config on each request, so a CA
// deployed or reconfigured after startup is picked up without a restart -
// exactly like the dashboard's /api/csr/sign path.
func buildMSCA(store envfile.Store, logger *slog.Logger) (http.Handler, string, error) {
	content, saved, err := store.Load()
	if err != nil {
		return nil, "", err
	}
	if !saved {
		return nil, "", nil
	}
	env := envfile.Parse(content)
	if !strings.EqualFold(env["VMSCA_ENABLE"], "true") {
		return nil, "", nil
	}
	user, pass := env["VMSCA_USERNAME"], env["VMSCA_PASSWORD"]
	if user == "" || pass == "" {
		return nil, "", fmt.Errorf("VMSCA_USERNAME and VMSCA_PASSWORD must be set")
	}
	port := env["VMSCA_PORT"]
	if port == "" {
		port = "8444"
	}

	sign := func(ctx context.Context, csr []byte) ([]byte, error) {
		content, saved, err := store.Load()
		if err != nil {
			return nil, err
		}
		if !saved {
			return nil, fmt.Errorf("no configuration saved")
		}
		return deploy.SignCSR(ctx, envfile.Parse(content), csr)
	}
	caChain := func() ([]byte, error) {
		content, _, err := store.Load()
		if err != nil {
			return nil, err
		}
		dir := envfile.Parse(content)["CA_DATA_DIR"]
		inter, err := os.ReadFile(filepath.Join(dir, "certs", "intermediate_ca.crt"))
		if err != nil {
			return nil, err
		}
		root, err := os.ReadFile(filepath.Join(dir, "certs", "root_ca.crt"))
		if err != nil {
			return nil, err
		}
		return append(inter, root...), nil
	}

	h := msca.New(msca.Config{Username: user, Password: pass, Template: env["VMSCA_TEMPLATE"]}, sign, caChain, logger)
	return h, ":" + port, nil
}
