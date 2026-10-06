package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The gcp log format exists because Google Cloud Logging reads "severity" and
// "message", while slog writes "level" and "msg". Without the rename every line
// files under DEFAULT severity, so a `severity>=ERROR` filter matches nothing and
// an alert on the error rate never fires — a failure that is silent in exactly the
// direction that matters, which is why it is worth a test.
func TestNewLogger_Format(t *testing.T) {
	cases := map[string]struct {
		format      string
		wantLevel   string // the key carrying the level
		wantMessage string // the key carrying the message
	}{
		"json is unchanged": {format: "json", wantLevel: "level", wantMessage: "msg"},
		"gcp renames both":  {format: "gcp", wantLevel: "severity", wantMessage: "message"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			line := captureStdout(t, func() {
				newLogger("info", tc.format).Error("something broke", "order", "abc")
			})

			var got map[string]any
			if err := json.Unmarshal([]byte(line), &got); err != nil {
				t.Fatalf("the log line is not JSON: %q", line)
			}
			if got[tc.wantLevel] != "ERROR" {
				t.Errorf("%q = %v, want ERROR (line: %s)", tc.wantLevel, got[tc.wantLevel], line)
			}
			if got[tc.wantMessage] != "something broke" {
				t.Errorf("%q = %v, want the message (line: %s)", tc.wantMessage, got[tc.wantMessage], line)
			}
			// Renamed, not duplicated: a line carrying both shows the message twice
			// in a console that understands neither.
			if tc.format == "gcp" {
				if _, dup := got["level"]; dup {
					t.Error("the gcp format still emits level")
				}
				if _, dup := got["msg"]; dup {
					t.Error("the gcp format still emits msg")
				}
			}
			// Attributes are untouched either way — only the two reserved keys move.
			if got["order"] != "abc" {
				t.Errorf("attributes were disturbed: %s", line)
			}
		})
	}
}

func TestNewLogger_Level(t *testing.T) {
	line := captureStdout(t, func() {
		newLogger("warn", "json").Info("not important enough")
	})
	if line != "" {
		t.Errorf("an info line was emitted at warn level: %q", line)
	}
}

// captureStdout runs fn with os.Stdout redirected, returning what was written.
// The logger writes to os.Stdout by name rather than to an injected writer, which
// is right for the production path and means a test has to redirect it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	fn()
	w.Close()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return strings.TrimSpace(sb.String())
}
