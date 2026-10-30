package main

import (
	"crypto/subtle"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	superUserName    = "super"
	superPassEnv     = "G711_SUPER_PASSWORD"
	superPassFile    = "super.pass"
	superMinPassLen  = 12
	superMaxPassLen  = 72 // bcrypt input limit
	superMaxFailures = 5
	superLockout     = 15 * time.Minute
)

// superAuth guards the user=super features with HTTP Basic auth. Only a
// bcrypt hash is persisted, in the analytics directory (never in the repo).
// The first password is seeded from the G711_SUPER_PASSWORD environment
// variable when no hash file exists yet.
type superAuth struct {
	path string
	now  func() time.Time

	mu        sync.Mutex
	hash      []byte
	failures  int
	lockUntil time.Time
}

func newSuperAuth(dir string, now func() time.Time) *superAuth {
	s := &superAuth{path: filepath.Join(dir, superPassFile), now: now}
	if b, err := os.ReadFile(s.path); err == nil {
		s.hash = []byte(strings.TrimSpace(string(b)))
	} else if pw := os.Getenv(superPassEnv); pw != "" {
		if err := s.setPassword(pw); err != nil {
			s.hash = nil
		}
	}
	return s
}

func (s *superAuth) setPassword(pw string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, h, 0o600); err != nil {
		return err
	}
	s.hash = h
	return nil
}

// check verifies the password, applying a lockout after repeated failures.
func (s *superAuth) check(pw string) (ok, locked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hash == nil {
		return false, false
	}
	if s.now().Before(s.lockUntil) {
		return false, true
	}
	if bcrypt.CompareHashAndPassword(s.hash, []byte(pw)) == nil {
		s.failures = 0
		return true, false
	}
	if s.failures++; s.failures >= superMaxFailures {
		s.failures = 0
		s.lockUntil = s.now().Add(superLockout)
	}
	return false, false
}

// authorize enforces Basic auth for user "super"; it writes the response and
// returns false when access is denied. Callers must only use it on requests
// carrying user=super.
func (s *superAuth) authorize(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	configured := s.hash != nil
	s.mu.Unlock()
	if !configured {
		http.Error(w, "super user is not configured", http.StatusForbidden)
		return false
	}
	user, pw, ok := r.BasicAuth()
	if ok && subtle.ConstantTimeCompare([]byte(user), []byte(superUserName)) == 1 {
		good, locked := s.check(pw)
		if good {
			return true
		}
		if locked {
			http.Error(w, "too many failed attempts; try again later", http.StatusTooManyRequests)
			return false
		}
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="Site information", charset="UTF-8"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
	return false
}

// changePassword verifies the current password and stores a new one.
func (s *superAuth) changePassword(current, next string) (string, int) {
	if len(next) < superMinPassLen {
		return "new password must be at least 12 characters", http.StatusBadRequest
	}
	if len(next) > superMaxPassLen {
		return "new password must be at most 72 bytes", http.StatusBadRequest
	}
	good, locked := s.check(current)
	if locked {
		return "too many failed attempts; try again later", http.StatusTooManyRequests
	}
	if !good {
		return "current password is incorrect", http.StatusForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.setPassword(next); err != nil {
		return "failed to save password", http.StatusInternalServerError
	}
	return "", http.StatusOK
}

// sameOrigin rejects cross-site form posts when the browser sends Origin.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	return strings.HasSuffix(o, "://"+r.Host)
}

func (a *analyticsStore) handlePassword(w http.ResponseWriter, r *http.Request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := r.PostFormValue("new")
	if next != r.PostFormValue("confirm") {
		http.Error(w, "new password and confirmation do not match", http.StatusBadRequest)
		return
	}
	if msg, code := a.super.changePassword(r.PostFormValue("current"), next); code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	a.logger.Printf("analytics: super user password changed")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("Password changed. Reload the page and sign in with the new password.\n"))
}
