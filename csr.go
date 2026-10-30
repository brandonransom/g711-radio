package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// isSuperUser reports the UI-only user=super flag. It is not authentication.
func isSuperUser(r *http.Request) bool {
	return r.URL.Query().Get("user") == "super"
}

var csrFileSafe = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// generateCSR creates a new RSA key and a PEM certificate signing request for
// the given common name and subject alternative names.
func generateCSR(cn string, sans []string, org, country, state, locality string, bits int) (csrPEM, keyPEM []byte, err error) {
	cn = strings.TrimSpace(cn)
	if cn == "" {
		return nil, nil, fmt.Errorf("common name is required")
	}
	if bits != 2048 && bits != 3072 && bits != 4096 {
		return nil, nil, fmt.Errorf("key size must be 2048, 3072, or 4096")
	}
	subject := pkix.Name{CommonName: cn}
	if org = strings.TrimSpace(org); org != "" {
		subject.Organization = []string{org}
	}
	if country = strings.ToUpper(strings.TrimSpace(country)); country != "" {
		if len(country) != 2 {
			return nil, nil, fmt.Errorf("country must be a 2-letter code")
		}
		subject.Country = []string{country}
	}
	if state = strings.TrimSpace(state); state != "" {
		subject.Province = []string{state}
	}
	if locality = strings.TrimSpace(locality); locality != "" {
		subject.Locality = []string{locality}
	}
	tmpl := &x509.CertificateRequest{Subject: subject, SignatureAlgorithm: x509.SHA256WithRSA}
	seen := map[string]bool{}
	for _, name := range append([]string{cn}, sans...) {
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		if ip := net.ParseIP(name); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, name)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, nil, err
	}
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return csrPEM, keyPEM, nil
}

// handleCSR generates a CSR, stores the private key under the analytics
// directory (never sent to the browser), and returns the CSR as a download.
func (a *analyticsStore) handleCSR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	privateHeaders(w)
	if !isSuperUser(r) || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !a.super.authorize(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	bits := 2048
	if v := r.PostFormValue("bits"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &bits); err != nil {
			http.Error(w, "invalid key size", http.StatusBadRequest)
			return
		}
	}
	sans := strings.FieldsFunc(r.PostFormValue("sans"), func(c rune) bool { return c == ',' || c == '\n' || c == '\r' || c == ' ' })
	cn := strings.TrimSpace(r.PostFormValue("cn"))
	csrPEM, keyPEM, err := generateCSR(cn, sans, r.PostFormValue("org"), r.PostFormValue("country"), r.PostFormValue("state"), r.PostFormValue("locality"), bits)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dir := filepath.Join(a.cfg.Dir, "csr")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		a.logger.Printf("csr: create dir: %v", err)
		http.Error(w, "failed to store private key", http.StatusInternalServerError)
		return
	}
	base := csrFileSafe.ReplaceAllString(strings.ReplaceAll(cn, "*", "wildcard"), "_") + "-" + a.now().Format("20060102-150405")
	if err := os.WriteFile(filepath.Join(dir, base+".key"), keyPEM, 0o600); err != nil {
		a.logger.Printf("csr: write key: %v", err)
		http.Error(w, "failed to store private key", http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, base+".csr"), csrPEM, 0o644); err != nil {
		a.logger.Printf("csr: write csr: %v", err)
	}
	a.logger.Printf("csr: generated %s (private key stored in %s)", base, dir)
	w.Header().Set("Content-Type", "application/pkcs10")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", base+".csr"))
	w.Write(csrPEM)
}
