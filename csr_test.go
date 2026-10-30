package main

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateCSR(t *testing.T) {
	csrPEM, keyPEM, err := generateCSR("radio.example.org", []string{"www.example.org", "203.0.113.5"}, "Org", "US", "", "", 2048)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(csrPEM)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		t.Fatalf("invalid CSR: %v", err)
	}
	if len(csr.DNSNames) != 2 || len(csr.IPAddresses) != 1 || csr.Subject.CommonName != "radio.example.org" {
		t.Fatalf("unexpected CSR contents: %+v", csr)
	}
	if !strings.Contains(string(keyPEM), "PRIVATE KEY") {
		t.Fatal("missing key")
	}
	if _, _, err := generateCSR("", nil, "", "", "", "", 2048); err == nil {
		t.Fatal("expected error for empty CN")
	}
}

func TestHandleCSRRequiresSuper(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(superPassEnv, "test-password-123")
	a := newTestStore(t, dir, &fakeClock{t: time.Date(2025, 3, 10, 9, 0, 0, 0, time.Local)})
	form := url.Values{"cn": {"radio.example.org"}}.Encode()
	do := func(target, pw string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if pw != "" {
			req.SetBasicAuth("super", pw)
		}
		rec := httptest.NewRecorder()
		a.handleCSR(rec, req)
		return rec
	}
	if rec := do("/stats/csr", "test-password-123"); rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	if rec := do("/stats/csr?user=super", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	if rec := do("/stats/csr?user=super", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	rec := do("/stats/csr?user=super", "test-password-123")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "BEGIN CERTIFICATE REQUEST") {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	keys, _ := filepath.Glob(filepath.Join(a.cfg.Dir, "csr", "*.key"))
	if len(keys) != 1 {
		t.Fatalf("expected one stored key, got %v", keys)
	}
	if _, err := os.Stat(keys[0]); err != nil {
		t.Fatal(err)
	}
}
