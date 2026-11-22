package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
		`stream: "T0": request: connection reused (true|false), dns -?\d+ ms, connect -?\d+ ms, tls -?\d+ ms, headers -?\d+ ms after the request`,
		`stream: "T0": bytes again after (7\d\d|[89]\d\d|\d{4,}) ms at 2 bytes`,
		`stream: "T0": 5 bytes in \d+ ms, \d+ KB/s, longest stall (7\d\d|[89]\d\d|\d{4,}) ms, the cache's trim before it \d+ ms, done`,
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
		return strings.Contains(string(b), "longest stall 0 ms, the cache's trim before it 0 ms, done")
	})
	b, _ := os.ReadFile(logPath)
	if strings.Contains(string(b), "no bytes for") || strings.Contains(string(b), "bytes again") {
		t.Errorf("a stall said:\n%s", b)
	}
}

// .stream at once when a download starts (its get line before the first byte) and when the bytes reach the
// firmware's start (startGate), not at the next statusEvery: the firmware starts the song from the line.
func TestStreamLineAtOnceAndAtTheStartGate(t *testing.T) {
	e := aheadEnv(t, false)
	e.s.statusEvery, e.s.poll = 2*time.Second, 2*time.Second
	total := 400 << 10
	hold := make(chan struct{})
	defer close(hold)
	first := make(chan struct{})
	e.src.body = func(string) (io.Reader, int64) {
		<-first
		return io.MultiReader(strings.NewReader(strings.Repeat("x", 330<<10)), held{hold}), int64(total)
	}
	list := songs(1, "flac")
	list[0].Duration, list[0].Size = 10, int64(total)
	if gate := startGate(int64(total), 10000); gate != 320<<10 {
		t.Fatalf("gate %d", gate)
	}
	e.s.start(list, 0)
	until(t, "the get line before a byte", func() bool {
		l := e.statusLines(t)
		return len(l) == 2 && strings.HasPrefix(l[1], "get\t0\t")
	})
	at := time.Now()
	close(first)
	until(t, "the line at the gate", func() bool {
		l := e.statusLines(t)
		return len(l) == 2 && strings.HasPrefix(l[1], fmt.Sprintf("get\t%d\t", 330<<10))
	})
	if d := time.Since(at); d > 500*time.Millisecond {
		t.Fatalf("the gate's line %v after the bytes", d)
	}
}

// held is a body that sends nothing more until hold closes, then ends.
type held struct{ hold chan struct{} }

func (h held) Read([]byte) (int, error) { <-h.hold; return 0, io.ErrUnexpectedEOF }

// cut is a body that sends n bytes of b and then fails, as a connection dropped mid-song.
type cut struct {
	r io.Reader
}

func (c *cut) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// A download cut mid-song, the song written in place (grow) and the player reading it: the bytes there
// are kept, never deleted under the player, its line says stall (the player waits, as for a slow server)
// rather than wait (the end of the bytes then the song's end), and the next try asks from where it stopped.
func TestADownloadCutMidSongResumesWhereItStopped(t *testing.T) {
	e := aheadEnv(t, true)
	e.s.backoff = []time.Duration{100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond}
	whole := strings.Repeat("abcdefgh", 4096) // 32 KB
	tries := 0
	var mu sync.Mutex
	e.src.ranged = func(u string, from int64) (*http.Response, error) {
		mu.Lock()
		tries++
		n := tries
		mu.Unlock()
		if n == 1 { // the first 10,000 bytes, then the connection goes
			return &http.Response{StatusCode: 200, ContentLength: int64(len(whole)),
				Body: io.NopCloser(&cut{strings.NewReader(whole[:10000])})}, nil
		}
		rest := whole[from:]
		return &http.Response{StatusCode: 206, ContentLength: int64(len(rest)), Body: io.NopCloser(strings.NewReader(rest))}, nil
	}
	list := songs(1, "flac")
	list[0].Size = int64(len(whole))
	e.s.start(list, 0)
	path := e.stream(0, "flac")
	until(t, "a stall at the bytes kept", func() bool {
		l := e.statusLines(t)
		return len(l) == 2 && strings.HasPrefix(l[1], "stall\t10000\t")
	})
	until(t, "the song whole", func() bool { b, _ := os.ReadFile(path); return string(b) == whole && e.s.whole(path) })
	e.src.mu.Lock()
	froms := append([]int64(nil), e.src.froms...)
	e.src.mu.Unlock()
	if len(froms) != 2 || froms[0] != 0 || froms[1] != 10000 {
		t.Fatalf("asked from %v; the second try from 10000", froms)
	}
}

// While the server has yet to answer, the line is get with nothing come, not stall: a stall is bytes
// that came and stopped (the firmware says why it waits from the state).
func TestNoStallBeforeTheFirstByte(t *testing.T) {
	e := aheadEnv(t, false)
	e.s.stallAfter, e.s.statusEvery = 20*time.Millisecond, 10*time.Millisecond
	answer := make(chan struct{})
	e.src.body = func(string) (io.Reader, int64) { <-answer; return strings.NewReader("123"), 3 }
	e.s.start(songs(1, "flac"), 0)
	until(t, "the get line", func() bool { l := e.statusLines(t); return len(l) == 2 && strings.HasPrefix(l[1], "get\t0\t") })
	time.Sleep(200 * time.Millisecond)
	l := e.statusLines(t)
	close(answer)
	if len(l) != 2 || !strings.HasPrefix(l[1], "get\t0\t") {
		t.Fatalf("waiting for the server: %q", l)
	}
}
