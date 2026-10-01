package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHallucinationFilterDefaults(t *testing.T) {
	f, err := newHallucinationFilter(nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		": Copyright Australian Broadcasting Corporation": noSpeechMarker,
		"Copyright Australian Broadcasting Corporation.":  noSpeechMarker,
		"Subtitles by the Amara.org community":            noSpeechMarker,
		"Thanks for watching! Thank you for watching.":    noSpeechMarker,
		" you":       noSpeechMarker,
		"Thank you.": noSpeechMarker,
		"♪ ♪":        noSpeechMarker,
		"Engine 4 responding. Thanks for watching!":               "Engine 4 responding.",
		"Copyright Australian Broadcasting Corporation Engine 4.": "Engine 4.",
		"Dispatch, thank you, Engine 4 clear.":                    "Dispatch, thank you, Engine 4 clear.",
		"Can you copy?":                                           "Can you copy?",
		"Copyright info is on the form.":                          "Copyright info is on the form.",
		"Engine 4 responding.":                                    "Engine 4 responding.",
		"Thanks for watchingtower 3":                              "Thanks for watchingtower 3",
	}
	for in, want := range cases {
		if got, _ := f.Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
	if _, changed := f.Apply("Engine 4 responding."); changed {
		t.Error("real traffic reported as filtered")
	}
}

func TestHallucinationFilterConfig(t *testing.T) {
	off := false
	f, err := newHallucinationFilter(&hallucinationFilterConfig{
		UseDefaults:  &off,
		Phrases:      []string{"Sheriff's Office test tone"},
		ExactPhrases: []string{"Okay."},
		Replacement:  "[Silence]",
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"Thanks for watching!":       "Thanks for watching!",
		"okay":                       "[Silence]",
		"SHERIFF'S OFFICE TEST TONE": "[Silence]",
		"Unit 2 en route. Sheriff's office test tone": "Unit 2 en route.",
	}
	for in, want := range cases {
		if got, _ := f.Apply(in); got != want {
			t.Errorf("Apply(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := newHallucinationFilter(&hallucinationFilterConfig{Phrases: []string{"!!"}}); err == nil {
		t.Error("expected error for a phrase with no words")
	}
	cfg := whisperConfig{HallucinationFilter: &hallucinationFilterConfig{ExactPhrases: []string{" "}}}
	if err := cfg.validate(); err == nil {
		t.Error("validate should reject an empty exact phrase")
	}
}

func TestWhisperPoolFiltersHallucinations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/inference":
			_ = json.NewEncoder(w).Encode(map[string]string{"text": " : Copyright Australian Broadcasting Corporation\n"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	pool, events := newTestPool(t, whisperConfig{RemoteServers: []string{srv.URL}})
	pool.Submit(transcriptJob{clipID: "c1", wavPath: writeTestWAV(t)})
	if ev := awaitTranscript(t, events, 5*time.Second); ev.Text != noSpeechMarker {
		t.Fatalf("published %q, want %q", ev.Text, noSpeechMarker)
	}
}
