package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/fakesonic"
)

// newClipRig is a daemon on firmware with the clip word, signed in to a server of four 200 s songs
// of 6,000,000 bytes (30,000 bytes a second), the server checked and up.
func newClipRig(t *testing.T, caps string) *catRig {
	t.Helper()
	songs := fakesonic.Library(4)
	for i := range songs {
		songs[i].Size, songs[i].Duration = 6_000_000, 200
	}
	r := newCatRig(t, caps, songs)
	r.clipInit()
	r.check(t)
	r.clipTick()
	t.Cleanup(r.clipClose)
	return r
}

// ask writes the firmware's request whole, as its clip thread does, and runs a tick of it.
func (r *catRig) ask(t *testing.T, req string) {
	t.Helper()
	p := filepath.Join(r.tmp, "q2-subsonic.clip")
	if err := os.WriteFile(p+".tmp", []byte(req), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		t.Fatal(err)
	}
	r.clipTick()
}

// answers is the app's answer file, its lines.
func (r *catRig) answers(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.tmp, "q2-subsonic.clips"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func (r *catRig) clipFiles() []string {
	ents, _ := os.ReadDir(filepath.Join(r.dir, "clips"))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// checkClip says whether the clip at path holds the song's bytes from at, as many as 28 s take.
func checkClip(t *testing.T, path string, at int64) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(30_000) * (25_000 + clipMargin.Milliseconds()) / 1000; int64(len(b)) != want {
		t.Fatalf("%s: %d bytes, want %d", path, len(b), want)
	}
	for i := range int64(len(b)) {
		if b[i] != fakesonic.Media(at+i) {
			t.Fatalf("%s: byte %d is the song's %d, not %d", path, i, b[i], at+i)
		}
	}
}

// A request for two clips: each fetched from the byte of its start (Range), written whole to the
// clips folder, and answered done with where its file starts in the song.
func TestClipsTwoInPlace(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up"}) {
		t.Fatal("at start:", a)
	}
	r.ask(t, "#Q2CLIP/1\tseq=7\n0\ts1\t60000\t25000\n1\ts2\t90000\t25000\n")
	r.bg.Wait()
	dir := filepath.Join(r.dir, "clips")
	want := []string{"#Q2CLIPS/1\tserver=up",
		"7\t0\tdone\t60000\t" + filepath.Join(dir, "s1.flac"),
		"7\t1\tdone\t90000\t" + filepath.Join(dir, "s2.flac")}
	if a := r.answers(t); !slices.Equal(a, want) {
		t.Fatalf("answers %q, want %q", a, want)
	}
	checkClip(t, filepath.Join(dir, "s1.flac"), 1_800_000)
	checkClip(t, filepath.Join(dir, "s2.flac"), 2_700_000)
	r.f.Mu.Lock()
	ranges := slices.Clone(r.f.Ranges)
	r.f.Mu.Unlock()
	if slices.Sort(ranges); !slices.Equal(ranges, []string{"s1 1800000", "s2 2700000"}) {
		t.Fatal(ranges)
	}
	if !slices.Equal(r.clipFiles(), []string{"s1.flac", "s2.flac"}) {
		t.Fatal(r.clipFiles())
	}
	log, _ := os.ReadFile(r.file("log.txt"))
	if !strings.Contains(string(log), "clips: s1 fetched, 840000 bytes") || !strings.Contains(string(log), "the range honoured") {
		t.Fatal(string(log))
	}
}

// A server that ignores Range sends the song from its start: the app reads up to the clip's start,
// keeps what follows, and answers with where its file starts all the same.
func TestClipsServerIgnoresRange(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.f.Mu.Lock()
	r.f.NoRange = true
	r.f.Mu.Unlock()
	r.ask(t, "#Q2CLIP/1\tseq=3\n0\ts3\t60000\t25000\n")
	r.bg.Wait()
	path := filepath.Join(r.dir, "clips", "s3.flac")
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up", "3\t0\tdone\t60000\t" + path}) {
		t.Fatal(a)
	}
	checkClip(t, path, 1_800_000)
	log, _ := os.ReadFile(r.file("log.txt"))
	if !strings.Contains(string(log), "the range ignored") {
		t.Fatal(string(log))
	}
}

// A new seq supersedes the request before it: its fetch still running is cancelled, and its clip
// neither answered nor kept.
func TestClipsNewSeqCancels(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.f.Mu.Lock()
	r.f.Hold, r.f.HoldBytes = "s1", 64<<10
	r.f.Mu.Unlock()
	r.ask(t, "#Q2CLIP/1\tseq=1\n0\ts1\t60000\t25000\n")
	for i := 0; ; i++ {
		r.f.Mu.Lock()
		n := len(r.f.Ranges)
		r.f.Mu.Unlock()
		if n == 1 {
			break
		}
		if i > 500 {
			t.Fatal("the first clip never asked of the server")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a := r.answers(t); len(a) != 2 || !strings.HasPrefix(a[1], "1\t0\tget\t") {
		t.Fatal("while it is fetched:", a)
	}
	r.ask(t, "#Q2CLIP/1\tseq=2\n0\ts2\t30000\t25000\n")
	r.bg.Wait()
	r.f.Mu.Lock()
	cut := r.f.Cut
	r.f.Mu.Unlock()
	path := filepath.Join(r.dir, "clips", "s2.flac")
	if a := r.answers(t); cut != 1 || !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up", "2\t0\tdone\t30000\t" + path}) {
		t.Fatal(cut, a)
	}
	if !slices.Equal(r.clipFiles(), []string{"s2.flac"}) {
		t.Fatal(r.clipFiles())
	}
}

// A request with no lines (the contest over) deletes every clip.
func TestClipsEmptyRequestDeletes(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.ask(t, "#Q2CLIP/1\tseq=7\n0\ts1\t60000\t25000\n1\ts2\t90000\t25000\n")
	r.bg.Wait()
	if len(r.clipFiles()) != 2 {
		t.Fatal(r.clipFiles())
	}
	r.ask(t, "#Q2CLIP/1\tseq=7\n")
	r.bg.Wait()
	if a := r.answers(t); len(r.clipFiles()) != 0 || !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up"}) {
		t.Fatal(r.clipFiles(), a)
	}
}

// The folder holds the clips the request names, two at most, and what is being written: a third
// line is answered fail, a file the request doesn't name goes, and so does a clip it stops naming.
func TestClipsFolderCapped(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	dir := filepath.Join(r.dir, "clips")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "old.mp3"), []byte("left by a run before"), 0o644)
	r.ask(t, "#Q2CLIP/1\tseq=5\n0\ts1\t60000\t25000\n1\ts2\t60000\t25000\n2\ts3\t60000\t25000\n")
	r.bg.Wait()
	a := r.answers(t)
	if len(a) != 4 || !strings.HasPrefix(a[1], "5\t0\tdone\t") || !strings.HasPrefix(a[2], "5\t1\tdone\t") || a[3] != "5\t2\tfail\t0\t" {
		t.Fatal(a)
	}
	if !slices.Equal(r.clipFiles(), []string{"s1.flac", "s2.flac"}) {
		t.Fatal(r.clipFiles())
	}
	r.ask(t, "#Q2CLIP/1\tseq=5\n1\ts2\t60000\t25000\n3\ts4\t60000\t25000\n")
	r.bg.Wait()
	if !slices.Equal(r.clipFiles(), []string{"s2.flac", "s4.flac"}) {
		t.Fatal(r.clipFiles())
	}
	if a := r.answers(t); len(a) != 3 || !strings.HasPrefix(a[1], "5\t1\tdone\t") || !strings.HasPrefix(a[2], "5\t3\tdone\t") {
		t.Fatal(a)
	}
}

// A clip fetch leaves the user's stream alone: its run goes on, nothing is reported as played, and
// none of the stream's files is written. While the user's song arrives slower than it plays, the
// clip waits, and goes on once it no longer does.
func TestClipsLeaveTheStreamAlone(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	done := make(chan struct{})
	r.stream.mu.Lock()
	r.stream.done, r.stream.cancel = done, func() { close(done) }
	r.stream.mu.Unlock()
	r.stream.xfer.Store(true)
	r.stream.slow.Store(true)
	r.ask(t, "#Q2CLIP/1\tseq=9\n0\ts1\t60000\t25000\n")
	time.Sleep(300 * time.Millisecond)
	if n := r.calls("stream"); n != 0 {
		t.Fatal("a clip fetched while the user's song arrives slowly:", n)
	}
	r.stream.slow.Store(false)
	r.bg.Wait()
	if a := r.answers(t); len(a) != 2 || !strings.HasPrefix(a[1], "9\t0\tdone\t") {
		t.Fatal(a)
	}
	if !r.stream.active() || r.calls("scrobble") != 0 {
		t.Fatal("the stream stopped, or a play reported:", r.stream.active(), r.calls("scrobble"))
	}
	for _, f := range []string{"q2-subsonic.play", "q2-subsonic.stream", "q2-subsonic.queue"} {
		if exists(filepath.Join(r.tmp, f)) {
			t.Fatal(f, "written")
		}
	}
	if exists(filepath.Join(r.card, ".subsonic", "stream")) {
		t.Fatal("the stream's folder written")
	}
}

// With the server down the header says so, and a clip asked for all the same is answered fail
// without a request for it.
func TestClipsServerDown(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.check(t)
	r.clipTick()
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=down"}) {
		t.Fatal(a)
	}
	r.ask(t, "#Q2CLIP/1\tseq=4\n0\ts1\t60000\t25000\n")
	r.bg.Wait()
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=down", "4\t0\tfail\t0\t"}) {
		t.Fatal(a)
	}
	if n := r.calls("stream"); n != 0 {
		t.Fatal(n)
	}
	log, _ := os.ReadFile(r.file("log.txt"))
	if !strings.Contains(string(log), "clips: s1 failed: the server is down") {
		t.Fatal(string(log))
	}
}

// Without the firmware's clip word the app writes no answer file and fetches nothing; with it, the
// file goes at the app's exit.
func TestClipsNeedTheWord(t *testing.T) {
	r := newClipRig(t, "playlist catalogue")
	r.ask(t, "#Q2CLIP/1\tseq=7\n0\ts1\t60000\t25000\n")
	r.bg.Wait()
	if exists(filepath.Join(r.tmp, "q2-subsonic.clips")) || r.calls("stream") != 0 {
		t.Fatal("clips without the word")
	}
	w := newClipRig(t, "playlist catalogue clip")
	w.clipClose()
	if exists(filepath.Join(w.tmp, "q2-subsonic.clips")) {
		t.Fatal("the answer file kept at exit")
	}
}
