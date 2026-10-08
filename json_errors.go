package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// jsonErrorContextChars is how much of the file before the error position is
// shown when a config file fails to parse.
const jsonErrorContextChars = 50

var unknownFieldPattern = regexp.MustCompile(`^json: unknown field "(.*)"$`)

// decodeJSONFile decodes data (the full contents of path) into v and, on
// failure, returns an error that names the line and column of the problem plus
// the text just before it. Set showContext to false for files holding secrets.
func decodeJSONFile(path string, data []byte, v any, strict, showContext bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return describeJSONError(path, data, err, showContext)
	}
	return nil
}

// describeJSONError wraps a JSON decode error with its location in data.
func describeJSONError(path string, data []byte, err error, showContext bool) error {
	offset, note := jsonErrorOffset(data, err)
	if offset < 0 {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	line, col := lineAndColumn(data, offset)
	msg := fmt.Sprintf("decode %s: line %d, column %d: %v", path, line, col, err)
	if note != "" {
		msg += " (" + note + ")"
	}
	if showContext {
		msg += "\n    near: " + jsonErrorContext(data, offset) + "  <-- here"
	}
	return &jsonLocatedError{msg: msg, err: err}
}

type jsonLocatedError struct {
	msg string
	err error
}

func (e *jsonLocatedError) Error() string { return e.msg }
func (e *jsonLocatedError) Unwrap() error { return e.err }

// jsonErrorOffset returns the byte offset in data just past the point where
// decoding failed, or -1 when the error carries no usable position.
func jsonErrorOffset(data []byte, err error) (int, string) {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr):
		return clampOffset(data, syntaxErr.Offset), ""
	case errors.As(err, &typeErr):
		return clampOffset(data, typeErr.Offset), ""
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return len(bytes.TrimRight(data, " \t\r\n")), "file ends early; check for a missing closing bracket or brace"
	}
	// Unknown-field errors carry no offset; locate the key in the text.
	if m := unknownFieldPattern.FindStringSubmatch(err.Error()); m != nil {
		key, _ := json.Marshal(m[1])
		re := regexp.MustCompile(regexp.QuoteMeta(string(key)) + `\s*:`)
		locs := re.FindAllIndex(data, -1)
		if len(locs) == 0 {
			return -1, ""
		}
		note := ""
		if len(locs) > 1 {
			note = fmt.Sprintf("first of %d occurrences of this key", len(locs))
		}
		return locs[0][0] + len(key), note
	}
	return -1, ""
}

func clampOffset(data []byte, off int64) int {
	if off < 0 {
		return 0
	}
	if off > int64(len(data)) {
		return len(data)
	}
	return int(off)
}

// lineAndColumn converts a byte offset just past the error into 1-based line
// and column numbers of the last byte read.
func lineAndColumn(data []byte, offset int) (int, int) {
	pos := offset - 1
	if pos < 0 {
		return 1, 1
	}
	// Point at the last meaningful byte rather than a trailing newline.
	for pos > 0 && (data[pos] == '\n' || data[pos] == '\r') {
		pos--
	}
	before := data[:pos]
	line := bytes.Count(before, []byte{'\n'}) + 1
	col := len([]rune(string(before[bytes.LastIndexByte(before, '\n')+1:]))) + 1
	return line, col
}

// jsonErrorContext returns up to jsonErrorContextChars characters ending at
// offset, on a single line.
func jsonErrorContext(data []byte, offset int) string {
	runes := []rune(string(data[:offset]))
	start := len(runes) - jsonErrorContextChars
	prefix := "..."
	if start <= 0 {
		start, prefix = 0, ""
	}
	s := string(runes[start:])
	s = strings.NewReplacer("\r\n", `\n`, "\n", `\n`, "\r", `\n`, "\t", " ").Replace(s)
	return prefix + s
}
