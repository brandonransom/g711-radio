package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDescribeJSONErrorLocations(t *testing.T) {
	type cfg struct {
		Name  string `json:"name"`
		Port  int    `json:"port"`
		Inner struct {
			Codec string `json:"codec"`
		} `json:"inner"`
	}
	cases := []struct {
		name     string
		input    string
		strict   bool
		wantLine string
		wantNear string
		wantNote string
	}{
		{
			name:     "missing comma",
			input:    "{\n  \"name\": \"a\"\n  \"port\": 5\n}",
			wantLine: "line 3, column 3",
			wantNear: `"name": "a"\n  "`,
		},
		{
			name:     "trailing comma",
			input:    "{\n  \"name\": \"a\",\n  \"port\": 5,\n}",
			wantLine: "line 4, column 1",
			wantNear: `"port": 5,\n}`,
		},
		{
			name:     "wrong type",
			input:    "{\n  \"name\": \"a\",\n  \"port\": \"5004\"\n}",
			wantLine: "line 3, column 16",
			wantNear: `"port": "5004"`,
		},
		{
			name:     "unknown field",
			input:    "{\n  \"name\": \"a\",\n  \"inner\": {\"codek\": \"pcmu\"}\n}",
			strict:   true,
			wantLine: "line 3, column 19",
			wantNear: `"inner": {"codek"`,
		},
		{
			name:     "truncated",
			input:    "{\n  \"name\": \"a\",\n  \"port\": 5\n",
			wantLine: "line 3, column 11",
			wantNote: "file ends early",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v cfg
			err := decodeJSONFile("test.json", []byte(tc.input), &v, tc.strict, true)
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "decode test.json: "+tc.wantLine) {
				t.Errorf("missing %q in:\n%s", tc.wantLine, msg)
			}
			if tc.wantNear != "" && !strings.Contains(msg, tc.wantNear+"  <-- here") {
				t.Errorf("missing context %q in:\n%s", tc.wantNear, msg)
			}
			if tc.wantNote != "" && !strings.Contains(msg, tc.wantNote) {
				t.Errorf("missing note %q in:\n%s", tc.wantNote, msg)
			}
		})
	}
}

func TestDescribeJSONErrorContextLength(t *testing.T) {
	input := `{"name": "` + strings.Repeat("x", 200) + `" "port": 1}`
	var v map[string]any
	err := decodeJSONFile("long.json", []byte(input), &v, false, true)
	if err == nil {
		t.Fatal("expected error")
	}
	near := err.Error()[strings.Index(err.Error(), "near: ")+len("near: "):]
	near = strings.TrimSuffix(near, "  <-- here")
	if !strings.HasPrefix(near, "...") || len([]rune(near)) != jsonErrorContextChars+3 {
		t.Fatalf("context = %q (%d runes)", near, len([]rune(near)))
	}
}

func TestDescribeJSONErrorHidesSecretsContext(t *testing.T) {
	input := "{\n  \"pfxPassword\": \"hunter2\"\n  \"x\": 1\n}"
	var v map[string]any
	err := decodeJSONFile("config.secrets.json", []byte(input), &v, false, false)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "near:") {
		t.Fatalf("secret leaked: %s", err)
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("missing line: %s", err)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatal("underlying error not unwrappable")
	}
}
