package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestResolveTLS(t *testing.T) {
	log := discardLogger()

	if resolveTLS("", "", log) {
		t.Error("no cert/key configured should not use TLS")
	}

	// A configured path that does not exist must fall back, not crash.
	if resolveTLS("/nonexistent/dashboard.crt", "/nonexistent/dashboard.key", log) {
		t.Error("missing cert file should fall back to HTTP")
	}

	// A malformed cert file must fall back too.
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resolveTLS(bad, bad, log) {
		t.Error("malformed cert should fall back to HTTP")
	}

	// A valid keypair must select TLS.
	certPath, keyPath := writeKeypair(t, dir)
	if !resolveTLS(certPath, keyPath, log) {
		t.Error("valid cert/key should use TLS")
	}
}

func writeKeypair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dashboard.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"dashboard.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "dashboard.crt")
	keyPath = filepath.Join(dir, "dashboard.key")

	certOut, _ := os.Create(certPath)
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, _ := os.Create(keyPath)
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
