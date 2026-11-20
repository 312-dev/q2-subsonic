package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func (d *tdaemon) logText(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(d.dir, "log.txt"))
	return string(b)
}

// The storage walk, the ten-minute job the user's CPU spikes line up with, says when it started and
// how long it took over how many entries; a quiet job that took no time says nothing, and one that
// took jobQuiet or more says what it did.
func TestJobsLogTheirStartAndDuration(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "storage", true)
	os.MkdirAll(filepath.Join(d.dir, "covers"), 0o755)
	for _, n := range []string{"a.jpg", "b.jpg"} {
		os.WriteFile(filepath.Join(d.dir, "covers", n), []byte("1234"), 0o644)
	}
	d.stOwn()
	log := d.logText(t)
	if !strings.Contains(log, "job storage walk: start") ||
		!regexp.MustCompile(`job storage walk: \d+ entries, [^\n]* in \d+`).MatchString(log) {
		t.Fatalf("log %q", log)
	}
	d.job("quick", true)("did %s", "nothing")
	slow := d.job("slow", true)
	time.Sleep(jobQuiet)
	slow("did %d things", 3)
	log = d.logText(t)
	if strings.Contains(log, "job quick") || !strings.Contains(log, "job slow: did 3 things in ") {
		t.Fatalf("log %q", log)
	}
}
