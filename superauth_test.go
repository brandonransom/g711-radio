package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSuperPasswordChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(superPassEnv, "initial-password-1")
	a := newTestStore(t, dir, &fakeClock{t: time.Date(2025, 3, 10, 9, 0, 0, 0, time.Local)})
	post := func(basicPW, current, next, confirm string) *httptest.ResponseRecorder {
		form := url.Values{"current": {current}, "new": {next}, "confirm": {confirm}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/stats/password?user=super", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("super", basicPW)
		rec := httptest.NewRecorder()
		a.handlePassword(rec, req)
		return rec
	}
	if rec := post("initial-password-1", "wrong", "brand-new-password", "brand-new-password"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong current: got %d", rec.Code)
	}
	if rec := post("initial-password-1", "initial-password-1", "short", "short"); rec.Code != http.StatusBadRequest {
		t.Fatalf("short: got %d", rec.Code)
	}
	if rec := post("initial-password-1", "initial-password-1", "brand-new-password", "different-password"); rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatch: got %d", rec.Code)
	}
	if rec := post("initial-password-1", "initial-password-1", "brand-new-password", "brand-new-password"); rec.Code != http.StatusOK {
		t.Fatalf("change: got %d: %s", rec.Code, rec.Body.String())
	}
	if ok, _ := a.super.check("initial-password-1"); ok {
		t.Fatal("old password still valid")
	}
	if ok, _ := a.super.check("brand-new-password"); !ok {
		t.Fatal("new password rejected")
	}
	b, _ := os.ReadFile(a.super.path)
	if strings.Contains(string(b), "brand-new-password") {
		t.Fatal("password stored in plaintext")
	}
}

func TestSuperLockout(t *testing.T) {
	t.Setenv(superPassEnv, "initial-password-1")
	a := newTestStore(t, t.TempDir(), &fakeClock{t: time.Date(2025, 3, 10, 9, 0, 0, 0, time.Local)})
	for i := 0; i < superMaxFailures; i++ {
		a.super.check("nope")
	}
	if ok, locked := a.super.check("initial-password-1"); ok || !locked {
		t.Fatalf("expected lockout, got ok=%v locked=%v", ok, locked)
	}
}
