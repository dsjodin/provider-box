// Package msca serves a minimal Microsoft ADCS "Certificate Authority Web
// Enrollment" (certsrv) surface in front of step-ca, so VCF / SDDC Manager can
// enroll certificates against this lab's CA using its Microsoft-CA integration.
// It implements only the handful of endpoints an ADCS web-enrollment client
// drives; every accepted CSR is signed synchronously through the injected
// Signer (deploy.SignCSR in production) - there is no pending/approval state.
package msca

import (
	"context"
	"crypto/subtle"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Signer signs a PEM-encoded CSR and returns a full-chain (leaf + intermediate)
// PEM certificate. In production this wraps deploy.SignCSR.
type Signer func(ctx context.Context, csrPEM []byte) ([]byte, error)

// CAChain returns the CA chain as concatenated PEM: the issuing CA
// (intermediate) first, then the root trust anchor.
type CAChain func() ([]byte, error)

type Config struct {
	Username string
	Password string
	Template string // accepted CertificateTemplate name; empty accepts any
}

type handler struct {
	cfg     Config
	sign    Signer
	caChain CAChain
	log     *slog.Logger

	mu   sync.Mutex
	seq  int
	byID map[int][]byte // ReqID -> issued leaf DER
}

// New builds the certsrv HTTP handler. Every route is behind HTTP Basic Auth
// against cfg.Username/cfg.Password.
func New(cfg Config, sign Signer, caChain CAChain, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &handler{cfg: cfg, sign: sign, caChain: caChain, log: logger, byID: map[int][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /certsrv/{$}", h.root)
	mux.HandleFunc("POST /certsrv/certfnsh.asp", h.submit)
	mux.HandleFunc("GET /certsrv/certnew.cer", h.cert)
	mux.HandleFunc("GET /certsrv/certnew.p7b", h.chain)
	mux.HandleFunc("GET /certsrv/certcarc.asp", h.renewals)
	return h.auth(mux)
}

// auth enforces Basic Auth. A missing or wrong credential returns 401 with a
// Basic challenge - the exact behavior an ADCS client probes for at /certsrv/.
func (h *handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(h.cfg.Username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(h.cfg.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="certsrv"`)
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// root answers the credential probe (GET /certsrv/) with a 200 once authed.
func (h *handler) root(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<html><body>certsrv</body></html>")
}

// submit handles the CSR POST (certfnsh.asp). It signs synchronously and
// returns an HTML page carrying the certnew.cer?ReqID=N& link the client parses.
func (h *handler) submit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("Mode") != "newreq" {
		http.Error(w, "unsupported Mode", http.StatusBadRequest)
		return
	}
	csr := r.PostForm.Get("CertRequest")
	if strings.TrimSpace(csr) == "" {
		http.Error(w, "missing CertRequest", http.StatusBadRequest)
		return
	}
	if h.cfg.Template != "" {
		if tmpl := parseTemplate(r.PostForm.Get("CertAttrib")); tmpl != "" && !strings.EqualFold(tmpl, h.cfg.Template) {
			http.Error(w, fmt.Sprintf("unknown certificate template %q", tmpl), http.StatusBadRequest)
			return
		}
	}
	chainPEM, err := h.sign(r.Context(), []byte(csr))
	if err != nil {
		h.log.Error("msca sign csr", "err", err)
		http.Error(w, "certificate request failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	leaf := firstCertDER(chainPEM)
	if leaf == nil {
		http.Error(w, "signer returned no certificate", http.StatusInternalServerError)
		return
	}
	id := h.put(leaf)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<html><body>Certificate Issued.<br>`+
		`<a href="certnew.cer?ReqID=%d&Enc=b64">Download certificate</a></body></html>`, id)
}

// cert serves an issued leaf (certnew.cer?ReqID=N) or the issuing CA cert
// (ReqID=CACert). Content-Type application/pkix-cert is what the client asserts.
func (h *handler) cert(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var der []byte
	if strings.EqualFold(q.Get("ReqID"), "CACert") {
		chain, err := h.caChain()
		if err != nil {
			http.Error(w, "CA chain unavailable", http.StatusInternalServerError)
			return
		}
		der = firstCertDER(chain)
	} else {
		id, err := strconv.Atoi(q.Get("ReqID"))
		if err != nil {
			http.Error(w, "bad ReqID", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		der = h.byID[id]
		h.mu.Unlock()
	}
	if der == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/pkix-cert")
	if strings.EqualFold(q.Get("Enc"), "bin") {
		_, _ = w.Write(der)
		return
	}
	_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// chain serves the CA chain as a certs-only PKCS#7 (certnew.p7b?ReqID=CACert).
func (h *handler) chain(w http.ResponseWriter, r *http.Request) {
	chain, err := h.caChain()
	if err != nil {
		http.Error(w, "CA chain unavailable", http.StatusInternalServerError)
		return
	}
	ders := allCertDER(chain)
	if len(ders) == 0 {
		http.Error(w, "CA chain unavailable", http.StatusInternalServerError)
		return
	}
	p7, err := degenerateP7B(ders)
	if err != nil {
		http.Error(w, "encode pkcs7", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-pkcs7-certificates")
	if strings.EqualFold(r.URL.Query().Get("Enc"), "bin") {
		_, _ = w.Write(p7)
		return
	}
	_ = pem.Encode(w, &pem.Block{Type: "PKCS7", Bytes: p7})
}

// renewals answers certcarc.asp with the nRenewals value the client parses.
// step-ca owns renewal; there is nothing to count, so it is always 0.
func (h *handler) renewals(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<html><body><script>var nRenewals=0;</script></body></html>")
}

func (h *handler) put(der []byte) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	h.byID[h.seq] = der
	return h.seq
}

// parseTemplate extracts the template from an ADCS CertAttrib value, which
// looks like "CertificateTemplate:VMware" optionally followed by CRLF-separated
// extra attributes.
func parseTemplate(attrib string) string {
	for _, line := range strings.FieldsFunc(attrib, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "CertificateTemplate:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstCertDER(pemBytes []byte) []byte {
	for {
		block, rest := pem.Decode(pemBytes)
		if block == nil {
			return nil
		}
		if block.Type == "CERTIFICATE" {
			return block.Bytes
		}
		pemBytes = rest
	}
}

func allCertDER(pemBytes []byte) [][]byte {
	var out [][]byte
	for {
		block, rest := pem.Decode(pemBytes)
		if block == nil {
			return out
		}
		if block.Type == "CERTIFICATE" {
			out = append(out, block.Bytes)
		}
		pemBytes = rest
	}
}
