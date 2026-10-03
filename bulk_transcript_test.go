package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBulkTranscriptRequestLimit(t *testing.T) {
	if maxBulkTranscriptRequest != 10000 || maxBulkQueue != 10000 {
		t.Fatal("bulk request and queue must support 10000 recordings")
	}
	s := &webrtcServer{
		logger:      log.New(io.Discard, "", 0),
		whisperPool: &whisperPool{},
	}
	for _, count := range []int{10000, 10001} {
		clips := make([]map[string]string, count)
		for i := range clips {
			clips[i] = map[string]string{"clipId": "", "audioUrl": strings.Repeat("x", 150)}
		}
		body, err := json.Marshal(map[string]any{"clips": clips})
		if err != nil {
			t.Fatal(err)
		}
		if len(body) <= 1<<20 {
			t.Fatal("test must exercise requests larger than the previous body limit")
		}
		r := httptest.NewRequest(http.MethodPost, "/transcripts/request-bulk", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.handleBulkTranscriptRequest(w, r)
		if count == 10000 {
			if w.Code != http.StatusAccepted {
				t.Fatalf("10000 clips: status %d: %s", w.Code, w.Body.String())
			}
			var response struct {
				NotFound int `json:"notFound"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.NotFound != count {
				t.Fatalf("processed %d clips, want %d", response.NotFound, count)
			}
		} else if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("10001 clips: status %d, want 413", w.Code)
		}
	}
}
