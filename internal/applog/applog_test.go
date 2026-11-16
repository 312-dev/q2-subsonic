package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	got := Redact("https://x/rest/ping?u=alice&t=abc&s=def&p=enc:1&apiKey=kkk&c=q2-subsonic")
	for _, bad := range []string{"alice", "abc", "def", "enc:1", "kkk"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q leaked in %q", bad, got)
		}
	}
	for _, kept := range []string{"https://x/rest/ping?", "u=***", "t=***", "apiKey=***", "c=q2-subsonic"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("lost %q: %q", kept, got)
		}
	}
}

// What an older build left in log.txt and log.txt.1 is redacted in place; the other lines stay as
// they were, and a log with nothing to redact is not written.
func TestScrub(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log.txt")
	old := "2026-11-01 10:00:00 start q2-v0.6\n" +
		"2026-11-01 10:00:01 GET https://music.example/rest/ping?c=q2-subsonic&f=json&s=5a1t5a1t5a1t&t=70ken70ken70ken&u=alice&v=1.13.0 -> 200\n" +
		"2026-11-01 10:00:02 sync: done\n"
	rotated := `2026-10-30 09:00:00 GET https://music.example/rest/ping?apiKey=K3YK3YK3Y&c=q2-subsonic -> Get "https://music.example/rest/ping?apiKey=K3YK3YK3Y&c=q2-subsonic": dial tcp: connection refused` + "\n"
	os.WriteFile(p, []byte(old), 0o600)
	os.WriteFile(p+".1", []byte(rotated), 0o600)
	if err := Open(p, 1<<20).Scrub(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	b1, _ := os.ReadFile(p + ".1")
	for _, bad := range []string{"5a1t5a1t5a1t", "70ken70ken70ken", "alice", "K3YK3YK3Y"} {
		if strings.Contains(string(b)+string(b1), bad) {
			t.Fatalf("%q left in\n%s%s", bad, b, b1)
		}
	}
	if want := strings.Replace(strings.Replace(strings.Replace(old, "5a1t5a1t5a1t", "***", 1), "70ken70ken70ken", "***", 1),
		"alice", "***", 1); string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal(st, err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp left:", err)
	}
	clean := filepath.Join(dir, "clean.txt")
	os.WriteFile(clean, []byte("2026-11-01 10:00:00 start q2-v0.7\n"), 0o600)
	when := time.Unix(1_700_000_000, 0)
	os.Chtimes(clean, when, when)
	if err := Open(clean, 1<<20).Scrub(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(clean); !st.ModTime().Equal(when) {
		t.Fatal("a clean log was written")
	}
	if err := Open(filepath.Join(dir, "none.txt"), 1<<20).Scrub(); err != nil {
		t.Fatal("no log:", err)
	}
}

// M1: a server address with a password in it (a basic-auth proxy) never reaches the log.
func TestRedactUserinfo(t *testing.T) {
	for _, in := range []string{
		"GET https://alice:proxySecret42@music.example/rest/ping?u=a -> 200",
		`Get "https://alice:proxySecret42@music.example:8443/rest/ping": EOF`,
		"https://alice:proxy@Secret42@music.example/x",
	} {
		got := Redact(in)
		if strings.Contains(got, "Secret42") || !strings.Contains(got, "music.example") {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestRotate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.txt")
	l := Open(p, 100)
	for i := 0; i < 20; i++ {
		l.Printf("line number %d", i)
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() > 100+60 {
		t.Fatalf("log.txt size %v err %v", st, err)
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatal("no rotated file:", err)
	}
}
