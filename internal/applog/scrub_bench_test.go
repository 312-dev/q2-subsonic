package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Scrub of a full log and its .1 (1 MB each, the app's limit), as each start ran it before serving.
func BenchmarkScrubFullLogs(b *testing.B) {
	dir := b.TempDir()
	l := Open(filepath.Join(dir, "log.txt"), 1<<20)
	line := "2026-11-22 21:36:08 GET https://navidrome-api.example/rest/stream?c=q2-subsonic&f=json&format=raw&id=3GPF7aDWyCigqZlqMDmwXz&s=***&t=***&u=***&v=1.13.0 -> 200\n"
	body := strings.Repeat(line, (1<<20)/len(line))
	os.WriteFile(l.path, []byte(body), 0o600)
	os.WriteFile(l.path+".1", []byte(body), 0o600)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Scrub()
	}
}

// The scrub runs once: a later start reads none of the log.
func TestScrubOnceRunsOnce(t *testing.T) {
	dir := t.TempDir()
	l := Open(filepath.Join(dir, "log.txt"), 1<<20)
	old := "x GET https://h/rest/ping?u=alice&p=secret -> 200\n"
	os.WriteFile(l.path, []byte(old), 0o600)
	if err := l.ScrubOnce(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(l.path); strings.Contains(string(b), "alice") {
		t.Fatalf("not scrubbed: %q", b)
	}
	os.WriteFile(l.path, []byte(old), 0o600) // as if written past Redact: a second start leaves it
	l.ScrubOnce()
	if b, _ := os.ReadFile(l.path); !strings.Contains(string(b), "alice") {
		t.Fatal("scrubbed again")
	}
}
