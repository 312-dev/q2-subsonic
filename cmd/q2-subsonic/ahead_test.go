package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/fw"
)

// aheadEnv is newEnv with the firmware's "ahead" word (and "grow" when asked), its .ahead and
// .stream files beside .play.
func aheadEnv(t *testing.T, grow bool) *env {
	t.Helper()
	e := newEnv(t, fw.Caps{Playlist: true, Ahead: true, Grow: grow})
	dir := filepath.Dir(e.play)
	e.s.aheadFile, e.s.statusFile = filepath.Join(dir, "ahead"), filepath.Join(dir, "stream")
	e.s.statusEvery, e.s.stallAfter = 10*time.Millisecond, time.Hour
	return e
}

// playSeq is the seq the app's .play names.
func (e *env) playSeq(t *testing.T) string {
	t.Helper()
	until(t, "play", func() bool { return exists(e.play) })
	b, _ := os.ReadFile(e.play)
	seq, _, _ := strings.Cut(string(b), " ")
	return seq
}

// ahead writes the firmware's .ahead: n, and the paths after the one playing.
func (e *env) ahead(t *testing.T, seq string, n int, paths ...string) {
	t.Helper()
	body := fmt.Sprintf("#Q2AHEAD/1\tseq=%s\tn=%d\n", seq, n)
	for _, p := range paths {
		body += p + "\n"
	}
	if err := os.WriteFile(e.s.aheadFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gate is a body that sends head bytes, then waits for open before the rest; closing it ends a
// read that waits, as a cancelled request's body does.
type gate struct {
	head, rest string
	open       chan struct{}
	sent       bool
	closed     chan struct{}
	once       sync.Once
}

func (g *gate) Close() error {
	g.once.Do(func() { close(g.closed) })
	return nil
}

func (g *gate) Read(p []byte) (int, error) {
	if !g.sent {
		g.sent = true
		return copy(p, g.head), nil
	}
	select {
	case <-g.open:
	case <-g.closed:
		return 0, io.ErrUnexpectedEOF
	}
	if g.rest == "" {
		return 0, io.EOF
	}
	n := copy(p, g.rest)
	g.rest = g.rest[n:]
	return n, nil
}

func (e *env) statusLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(e.s.statusFile)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// The firmware's .ahead says which songs come next in its play order (shuffle, Up Next): the app
// fetches those, not the list's next ones.
func TestAheadFetchesTheFirmwaresNext(t *testing.T) {
	e := aheadEnv(t, false)
	release := make(chan struct{})
	e.src.onGet = func() {
		if len(e.src.got()) == 1 {
			<-release // the first song waits until .ahead is there
		}
	}
	e.s.start(songs(6, "flac"), 0)
	seq := e.playSeq(t)
	os.WriteFile(e.playing, []byte(e.stream(0, "flac")+"\n"), 0o644)
	e.ahead(t, seq, 1, e.stream(4, "flac"), e.stream(1, "flac"))
	close(release)
	until(t, "s4 fetched", func() bool { return exists(e.stream(4, "flac")) })
	time.Sleep(50 * time.Millisecond)
	for _, id := range []string{"s1", "s2", "s3", "s5"} {
		if n := e.src.tries(id); n != 0 {
			t.Errorf("%s fetched %d times; .ahead's n=1 names s4 alone", id, n)
		}
	}
}

// Fewer songs named than n: the rest are the list's songs after the one playing not yet whole,
// which the firmware has not queued yet (s3 is queued, shuffled; s1 is the first not there).
func TestAheadFillsWithTheSongsNotQueued(t *testing.T) {
	e := aheadEnv(t, false)
	release := make(chan struct{})
	var seq string
	e.src.onGet = func() {
		got := e.src.got()
		switch {
		case len(got) == 1:
			<-release
		case strings.Contains(got[len(got)-1], ":s1:"): // the firmware queues s1 once it lands
			e.ahead(t, seq, 2, e.stream(3, "flac"), e.stream(1, "flac"))
		}
	}
	e.s.start(songs(6, "flac"), 0)
	seq = e.playSeq(t)
	os.WriteFile(e.playing, []byte(e.stream(0, "flac")+"\n"), 0o644)
	e.ahead(t, seq, 2, e.stream(3, "flac"))
	close(release)
	until(t, "s3 and s1 fetched", func() bool { return exists(e.stream(3, "flac")) && exists(e.stream(1, "flac")) })
	time.Sleep(50 * time.Millisecond)
	for _, id := range []string{"s2", "s4", "s5"} {
		if n := e.src.tries(id); n != 0 {
			t.Errorf("%s fetched %d times; n=2 is s3 and then s1", id, n)
		}
	}
}

// Download Ahead Off (n=0): only the song playing is fetched.
func TestAheadOffFetchesOnlyThePlayingSong(t *testing.T) {
	e := aheadEnv(t, false)
	release := make(chan struct{})
	e.src.onGet = func() {
		if len(e.src.got()) == 1 {
			<-release
		}
	}
	e.s.start(songs(4, "flac"), 0)
	seq := e.playSeq(t)
	os.WriteFile(e.playing, []byte(e.stream(0, "flac")+"\n"), 0o644)
	e.ahead(t, seq, 0, e.stream(1, "flac"))
	close(release)
	until(t, "s0 fetched", func() bool { return exists(e.stream(0, "flac")) })
	time.Sleep(50 * time.Millisecond)
	if got := e.src.got(); len(got) != 1 {
		t.Errorf("fetched %v; Off fetches the song playing alone", got)
	}
}

// An .ahead of another .play is an older queue's: the list's order stands.
func TestAheadOfAnotherQueueIgnored(t *testing.T) {
	e := aheadEnv(t, false)
	e.ahead(t, "1", 0)
	e.s.start(songs(3, "flac"), 0)
	until(t, "s2 fetched", func() bool { return exists(e.stream(2, "flac")) })
}

// A song that leaves the plan while it downloads is cancelled, its partial file deleted, and the
// new plan fetched.
func TestAheadChangeCancelsTheDownload(t *testing.T) {
	e := aheadEnv(t, false)
	g := &gate{head: "partial", rest: "-rest", open: make(chan struct{}), closed: make(chan struct{})}
	e.src.body = func(u string) (io.Reader, int64) {
		if strings.Contains(u, ":s1:") {
			return g, 12
		}
		b := "data-" + u
		return strings.NewReader(b), int64(len(b))
	}
	defer close(g.open)
	e.s.start(songs(5, "flac"), 0)
	seq := e.playSeq(t)
	os.WriteFile(e.playing, []byte(e.stream(0, "flac")+"\n"), 0o644)
	e.ahead(t, seq, 1, e.stream(1, "flac"))
	until(t, "s1 partial", func() bool { return exists(e.stream(1, "flac") + ".part") })
	e.ahead(t, seq, 1, e.stream(3, "flac"))
	until(t, "s3 fetched", func() bool { return exists(e.stream(3, "flac")) })
	if exists(e.stream(1, "flac")+".part") || exists(e.stream(1, "flac")) {
		t.Error("s1's partial file was kept after it left the plan")
	}
	if e.s.Err() != nil {
		t.Errorf("a cancelled download counted as a failure: %v", e.s.Err())
	}
}

// .stream says how far each planned song has come: get with its bytes while it arrives, done
// once whole.
func TestStreamStatusFile(t *testing.T) {
	e := aheadEnv(t, false)
	g := &gate{head: "1234567", rest: "890", open: make(chan struct{}), closed: make(chan struct{})}
	e.src.body = func(u string) (io.Reader, int64) {
		if strings.Contains(u, ":s0:") {
			return g, 10
		}
		b := "data-" + u
		return strings.NewReader(b), int64(len(b))
	}
	sg := songs(2, "flac")
	sg[0].Duration = 200
	e.s.start(sg, 0)
	seq := e.playSeq(t)
	e.ahead(t, seq, 0)
	want := fmt.Sprintf("get\t7\t10\t200000\t%s", e.stream(0, "flac"))
	until(t, "get line", func() bool {
		l := e.statusLines(t)
		return len(l) == 2 && l[0] == "#Q2STREAM/1\tseq="+seq && l[1] == want
	})
	close(g.open)
	want = fmt.Sprintf("done\t10\t10\t200000\t%s", e.stream(0, "flac"))
	until(t, "done line", func() bool {
		l := e.statusLines(t)
		return len(l) == 2 && l[1] == want
	})
	e.s.stop()
	if exists(e.s.statusFile) {
		t.Error(".stream kept after the stream stopped")
	}
}

// A download with no byte for stallAfter reads stall.
func TestStreamStatusStall(t *testing.T) {
	e := aheadEnv(t, false)
	e.s.stallAfter = 20 * time.Millisecond
	g := &gate{head: "12", rest: "3", open: make(chan struct{}), closed: make(chan struct{})}
	defer close(g.open)
	e.src.body = func(string) (io.Reader, int64) { return g, 3 }
	e.s.start(songs(1, "flac"), 0)
	until(t, "stall line", func() bool {
		l := e.statusLines(t)
		return len(l) == 2 && strings.HasPrefix(l[1], "stall\t2\t3\t")
	})
}

// With "grow" a song is written in place, listed in .growing while it arrives; a start after a
// crash deletes what .growing lists.
func TestGrowWritesInPlace(t *testing.T) {
	e := aheadEnv(t, true)
	g := &gate{head: "head", rest: "tail", open: make(chan struct{}), closed: make(chan struct{})}
	e.src.body = func(u string) (io.Reader, int64) {
		if strings.Contains(u, ":s0:") {
			return g, 8
		}
		b := "data-" + u
		return strings.NewReader(b), int64(len(b))
	}
	e.s.start(songs(2, "flac"), 0)
	journal := filepath.Join(e.s.dir(), ".growing")
	until(t, "in place", func() bool {
		b, _ := os.ReadFile(e.stream(0, "flac"))
		j, _ := os.ReadFile(journal)
		return string(b) == "head" && string(j) == e.stream(0, "flac")+"\n"
	})
	if exists(e.stream(0, "flac") + ".part") {
		t.Error("a .part was written with grow")
	}
	close(g.open)
	until(t, "whole", func() bool {
		b, _ := os.ReadFile(e.stream(0, "flac"))
		j, _ := os.ReadFile(journal)
		return string(b) == "headtail" && len(j) == 0
	})
}

func TestGrowingLeftByACrashIsDeleted(t *testing.T) {
	e := aheadEnv(t, true)
	cut := filepath.Join(e.s.dir(), "acct", "q", "a", "cut [x].flac")
	os.MkdirAll(filepath.Dir(cut), 0o755)
	os.WriteFile(cut, []byte("half"), 0o644)
	os.WriteFile(filepath.Join(e.s.dir(), ".growing"), []byte(cut+"\n"), 0o644)
	// a stream the firmware may still play: cleanLeft keeps the folder, not the cut file
	playing := filepath.Join(e.s.dir(), "0000 - T0 [s0].flac")
	os.WriteFile(playing, []byte("whole"), 0o644)
	os.WriteFile(e.playing, []byte(playing+"\n"), 0o644)
	e.s.cleanLeft()
	if !exists(playing) {
		t.Error("the song .playing names went")
	}
	if exists(cut) {
		t.Error("a file .growing lists survived a start")
	}
	if exists(filepath.Join(e.s.dir(), ".growing")) {
		t.Error(".growing survived a start")
	}
}
