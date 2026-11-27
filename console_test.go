package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLogBufferSplitsAndWraps(t *testing.T) {
	b := newLogBuffer()
	fmt.Fprint(b, "one\r\ntwo\nthr")
	if u := b.Since(0); len(u.Lines) != 2 || u.Lines[0] != "one" || u.Lines[1] != "two" || u.Next != 2 {
		t.Fatalf("got %+v", u)
	}
	fmt.Fprint(b, "ee\n")
	if u := b.Since(2); len(u.Lines) != 1 || u.Lines[0] != "three" || u.Next != 3 || u.Missed || u.Reset {
		t.Fatalf("got %+v", u)
	}
	if u := b.Since(3); len(u.Lines) != 0 || u.Next != 3 {
		t.Fatalf("idle poll: got %+v", u)
	}

	for i := 0; i < consoleBufferLines+10; i++ {
		fmt.Fprintf(b, "line %d\n", i)
	}
	u := b.Since(3)
	if !u.Missed || len(u.Lines) != consoleBufferLines || u.Next != uint64(3+consoleBufferLines+10) {
		t.Fatalf("after wrap: missed=%v lines=%d next=%d", u.Missed, len(u.Lines), u.Next)
	}
	if want := fmt.Sprintf("line %d", 10); u.Lines[0] != want {
		t.Fatalf("oldest kept line = %q, want %q", u.Lines[0], want)
	}
	if u := b.Since(0); u.Missed {
		t.Fatal("a first load is not a gap")
	}
	if u := b.Since(u.Next + 100); !u.Reset || len(u.Lines) != consoleBufferLines {
		t.Fatalf("restarted server: reset=%v lines=%d", u.Reset, len(u.Lines))
	}
}

func TestConsoleRequiresSuperLoginAndStreams(t *testing.T) {
	t.Setenv(superPassEnv, "initial-password-1")
	a := newTestStore(t, t.TempDir(), &fakeClock{t: time.Date(2025, 3, 10, 9, 0, 0, 0, time.Local)})
	a.console = newLogBuffer()
	fmt.Fprint(a.console, "first line\n")
	mux := http.NewServeMux()
	a.registerConsole(mux, "/stats")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(path string, authed bool) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if authed {
			req.SetBasicAuth("super", "initial-password-1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for _, p := range []string{"/stats/console", "/stats/console/stream"} {
		resp := get(p, false)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without login: got %d", p, resp.StatusCode)
		}
	}

	page := get("/stats/console", true)
	page.Body.Close()
	if csp := page.Header.Get("Content-Security-Policy"); page.StatusCode != http.StatusOK || !strings.Contains(csp, "script-src 'nonce-") {
		t.Fatalf("page: status %d csp %q", page.StatusCode, csp)
	}

	resp := get("/stats/console/stream", true)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	// nextEvent reads one complete server-sent event: its name ("" for the
	// default event) and data.
	nextEvent := func() (name, data string) {
		t.Helper()
		for sc.Scan() {
			switch line := sc.Text(); {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && data != "":
				return name, data
			}
		}
		t.Fatalf("stream ended: %v", sc.Err())
		return "", ""
	}
	next := func() consoleUpdate {
		t.Helper()
		for {
			name, data := nextEvent()
			if name != "" {
				continue
			}
			var u consoleUpdate
			if err := json.Unmarshal([]byte(data), &u); err != nil {
				t.Fatal(err)
			}
			return u
		}
	}
	if u := next(); len(u.Lines) != 1 || u.Lines[0] != "first line" {
		t.Fatalf("backlog: %+v", u)
	}
	fmt.Fprint(a.console, "second line\n")
	if u := next(); len(u.Lines) != 1 || u.Lines[0] != "second line" || u.Next != 2 {
		t.Fatalf("live line: %+v", u)
	}

	fmt.Fprint(a.console, "2026/11/27 12:52:50 archive sync: error: mkdir B:\\audio: The system cannot find the path specified.\n")
	deadline := time.After(10 * time.Second)
	for found := false; !found; {
		select {
		case <-deadline:
			t.Fatal("no issues event mentioning the archive problem")
		default:
		}
		name, data := nextEvent()
		if name != "issues" {
			continue
		}
		var list []issueView
		if err := json.Unmarshal([]byte(data), &list); err != nil {
			t.Fatal(err)
		}
		for _, v := range list {
			found = found || v.ID == "archive-unreachable|B:"
		}
	}

	a.console.Close()
	for sc.Scan() {
	}
}
