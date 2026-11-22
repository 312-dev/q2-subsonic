package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
)

// A download's timings in log.txt (the user's report of 2026-11-22: the progress stuck at 0.3 MB
// before a streamed song started): headers and first byte, a wait for bytes past stallLog said while
// it runs and again when bytes come, and a summary at the end. No credentials in any of it.
func TestADownloadsTimingsAndStallsAreLogged(t *testing.T) {
	e := aheadEnv(t, false)
	e.s.statusEvery = 50 * time.Millisecond
	logPath := filepath.Join(t.TempDir(), "log.txt")
	e.s.log = applog.Open(logPath, 1<<20)
	g := &gate{head: "12", rest: "345", open: make(chan struct{}), closed: make(chan struct{})}
	e.src.body = func(string) (io.Reader, int64) { return g, 5 }
	e.s.start(songs(1, "flac"), 0)
	read := func() string { b, _ := os.ReadFile(logPath); return string(b) }
	until(t, "the stall said while it runs", func() bool {
		return strings.Contains(read(), `stream: "T0": no bytes for `) && strings.Contains(read(), " ms at 2 bytes")
	})
	close(g.open)
	until(t, "the summary", func() bool { return strings.Contains(read(), `stream: "T0": 5 bytes in `) })
	log := read()
	for _, re := range []string{
		`stream: "T0": headers in \d+ ms, first byte in \d+ ms`,
		`stream: "T0": bytes again after (7\d\d|[89]\d\d|\d{4,}) ms at 2 bytes`,
		`stream: "T0": 5 bytes in \d+ ms, \d+ KB/s, longest stall (7\d\d|[89]\d\d|\d{4,}) ms, done`,
	} {
		if !regexp.MustCompile(re).MatchString(log) {
			t.Errorf("no line matching %s in\n%s", re, log)
		}
	}
	if n := strings.Count(log, "no bytes for"); n != 1 {
		t.Errorf("the one wait said %d times", n)
	}
}

// A download that never waits says no stall.
func TestASteadyDownloadSaysNoStall(t *testing.T) {
	e := aheadEnv(t, false)
	logPath := filepath.Join(t.TempDir(), "log.txt")
	e.s.log = applog.Open(logPath, 1<<20)
	e.s.start(songs(1, "flac"), 0)
	until(t, "the summary", func() bool {
		b, _ := os.ReadFile(logPath)
		return strings.Contains(string(b), "longest stall 0 ms, done")
	})
	b, _ := os.ReadFile(logPath)
	if strings.Contains(string(b), "no bytes for") || strings.Contains(string(b), "bytes again") {
		t.Errorf("a stall said:\n%s", b)
	}
}
