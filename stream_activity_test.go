package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestHandleStreamActivityReportsOnlyRecentPackets(t *testing.T) {
	now := time.Now()
	s := &webrtcServer{streams: map[string]*station{
		"live":  {lastPacketAt: now},
		"stale": {lastPacketAt: now.Add(-time.Minute)},
		"never": {},
	}}

	rec := httptest.NewRecorder()
	s.handleStreamActivity(rec, httptest.NewRequest(http.MethodGet, "/stream-activity", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Active []string `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if want := []string{"live"}; !reflect.DeepEqual(body.Active, want) {
		t.Errorf("active = %v, want %v", body.Active, want)
	}
}

func TestHandleStreamActivityRejectsPost(t *testing.T) {
	s := &webrtcServer{streams: map[string]*station{}}
	rec := httptest.NewRecorder()
	s.handleStreamActivity(rec, httptest.NewRequest(http.MethodPost, "/stream-activity", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
