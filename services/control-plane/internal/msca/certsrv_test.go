package msca

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func testCert(t *testing.T, cn string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// newTestHandler wires a handler whose signer returns a fixed leaf+intermediate
// chain and whose CA chain is intermediate+root, so tests need no step-ca.
func newTestHandler(t *testing.T) (http.Handler, []byte) {
	t.Helper()
	leaf := testCert(t, "vcenter.sddc.lab")
	inter := testCert(t, "labprovider Intermediate CA")
	root := testCert(t, "labprovider Root CA")
	sign := func(ctx context.Context, csr []byte) ([]byte, error) {
		return append(pemCert(leaf), pemCert(inter)...), nil
	}
	caChain := func() ([]byte, error) {
		return append(pemCert(inter), pemCert(root)...), nil
	}
	cfg := Config{Username: "vcf", Password: "s3cret", Template: "VMware"}
	return New(cfg, sign, caChain, nil), leaf
}

func do(t *testing.T, h http.Handler, method, target string, body url.Values, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if auth {
		r.SetBasicAuth("vcf", "s3cret")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthProbe(t *testing.T) {
	h, _ := newTestHandler(t)
	if w := do(t, h, "GET", "/certsrv/", nil, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth: want 401, got %d", w.Code)
	} else if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("missing Basic challenge: %q", w.Header().Get("WWW-Authenticate"))
	}
	if w := do(t, h, "GET", "/certsrv/", nil, true); w.Code != http.StatusOK {
		t.Fatalf("auth: want 200, got %d", w.Code)
	}
}

func TestEnrollmentFlow(t *testing.T) {
	h, wantLeaf := newTestHandler(t)

	form := url.Values{
		"Mode":        {"newreq"},
		"CertRequest": {"-----BEGIN CERTIFICATE REQUEST-----\nAAAA\n-----END CERTIFICATE REQUEST-----"},
		"CertAttrib":  {"CertificateTemplate:VMware"},
		"SaveCert":    {"yes"},
	}
	w := do(t, h, "POST", "/certsrv/certfnsh.asp", form, true)
	if w.Code != http.StatusOK {
		t.Fatalf("submit: want 200, got %d (%s)", w.Code, w.Body)
	}
	m := regexp.MustCompile(`certnew\.cer\?ReqID=(\d+)&`).FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("submit response has no ReqID link: %s", w.Body)
	}
	reqID := m[1]

	w = do(t, h, "GET", "/certsrv/certnew.cer?ReqID="+reqID+"&Enc=b64", nil, true)
	if ct := w.Header().Get("Content-Type"); ct != "application/pkix-cert" {
		t.Fatalf("certnew.cer Content-Type: got %q", ct)
	}
	block, _ := pem.Decode(w.Body.Bytes())
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("certnew.cer body is not a PEM certificate")
	}
	if string(block.Bytes) != string(wantLeaf) {
		t.Fatalf("certnew.cer returned the wrong certificate")
	}
}

func TestTemplateMismatch(t *testing.T) {
	h, _ := newTestHandler(t)
	form := url.Values{
		"Mode":        {"newreq"},
		"CertRequest": {"csr"},
		"CertAttrib":  {"CertificateTemplate:WebServer"},
	}
	if w := do(t, h, "POST", "/certsrv/certfnsh.asp", form, true); w.Code != http.StatusBadRequest {
		t.Fatalf("template mismatch: want 400, got %d", w.Code)
	}
}

func TestChainP7B(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, "GET", "/certsrv/certnew.p7b?ReqID=CACert&Enc=bin", nil, true)
	if ct := w.Header().Get("Content-Type"); ct != "application/x-pkcs7-certificates" {
		t.Fatalf("certnew.p7b Content-Type: got %q", ct)
	}
	certs := parseP7BCerts(t, w.Body.Bytes())
	if len(certs) != 2 {
		t.Fatalf("certnew.p7b: want 2 certs, got %d", len(certs))
	}
	for _, der := range certs {
		if _, err := x509.ParseCertificate(der); err != nil {
			t.Fatalf("certnew.p7b cert does not parse: %v", err)
		}
	}
}

func TestRenewals(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, "GET", "/certsrv/certcarc.asp", nil, true)
	if !regexp.MustCompile(`var nRenewals=(\d+);`).MatchString(w.Body.String()) {
		t.Fatalf("certcarc.asp missing nRenewals: %s", w.Body)
	}
}

// parseP7BCerts round-trips the degenerate PKCS#7 to prove it is well-formed
// and recovers the embedded certificates.
func parseP7BCerts(t *testing.T, der []byte) [][]byte {
	t.Helper()
	var ci contentInfo
	if _, err := asn1.Unmarshal(der, &ci); err != nil {
		t.Fatalf("outer ContentInfo: %v", err)
	}
	if !ci.ContentType.Equal(oidSignedData) {
		t.Fatalf("outer OID = %v, want signedData", ci.ContentType)
	}
	var sd signedData
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		t.Fatalf("SignedData: %v", err)
	}
	var out [][]byte
	rest := sd.Certificates.Bytes
	for len(rest) > 0 {
		var one asn1.RawValue
		var err error
		rest, err = asn1.Unmarshal(rest, &one)
		if err != nil {
			t.Fatalf("certificate: %v", err)
		}
		out = append(out, one.FullBytes)
	}
	return out
}
