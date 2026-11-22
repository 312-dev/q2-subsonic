package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

// settingsEnv is an env whose Streaming Quality is read as the daemon reads it (loadSettings: the card's
// settings.json, decoded), counting the reads.
func settingsEnv(tb testing.TB, reads *atomic.Int64) *env {
	dir := tb.TempDir()
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"stream":"original","cache":"2"}`), 0o644)
	t, _ := tb.(*testing.T)
	var e *env
	if t != nil {
		e = newEnv(t, fw.Caps{Playlist: true})
	} else {
		e = newEnvB(tb.(*testing.B))
	}
	e.s.quality = func() string { reads.Add(1); return loadSettings(dir).stream() }
	return e
}

// A list's start reads Streaming Quality once, not once a song: on the user's Q2 a Shuffle of the
// whole library (9,199 songs) read settings.json from the card 9,199 times before its first request.
func TestAListsStartReadsTheQualityOnce(t *testing.T) {
	var reads atomic.Int64
	e := settingsEnv(t, &reads)
	e.s.start(songs(9199, "flac"), 0)
	e.s.stop()
	if n := reads.Load(); n > 2 {
		t.Fatalf("Streaming Quality read %d times for one list's start", n)
	}
}

func benchStart(b *testing.B, n int) {
	var reads atomic.Int64
	e := settingsEnv(b, &reads)
	list := songs(n, "flac")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.s.start(list, 0)
		e.s.stop()
	}
	b.ReportMetric(float64(reads.Load())/float64(b.N), "settings-reads/start")
}

func BenchmarkListStart10(b *testing.B)   { benchStart(b, 10) }
func BenchmarkListStart1000(b *testing.B) { benchStart(b, 1000) }
func BenchmarkListStart9199(b *testing.B) { benchStart(b, 9199) }

// newEnvB is newEnv for a benchmark.
func newEnvB(b *testing.B) *env {
	e := &env{root: b.TempDir(), src: &fakeSrc{}, man: &library.Manifest{Songs: map[string]*library.Entry{}}, net: syncer.NewNetSlot()}
	tmp := b.TempDir()
	e.play, e.playing = tmp+"/play", tmp+"/playing"
	e.s = newStreamer(e.root, e.play, e.playing, fw.Caps{Playlist: true}, e.src, func() *library.Manifest { return e.man }, e.net, 5*time.Millisecond)
	e.s.gap = 0
	b.Cleanup(e.s.stop)
	return e
}

// The songs passed are deleted once each, not again on every pass of the run loop (once a second on
// the Q2): with the cache off and 498 songs behind the one playing, that was 498 deletes a second.
func TestPassedSongsAreDeletedOnce(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	var mu sync.Mutex
	removed := map[string]int{}
	e.s.removeHook = func(p string) { mu.Lock(); removed[p]++; mu.Unlock() }
	e.s.start(songs(600, "flac"), 0)
	os.WriteFile(e.playing, []byte(e.stream(500, "flac")+"\n"), 0o644)
	until(t, "the songs behind deleted", func() bool { mu.Lock(); defer mu.Unlock(); return len(removed) == 498 })
	time.Sleep(200 * time.Millisecond) // 40 passes at the test's 5 ms
	mu.Lock()
	defer mu.Unlock()
	for p, n := range removed {
		if n != 1 {
			t.Fatalf("%s deleted %d times", filepath.Base(p), n)
		}
	}
}

// plan on a 9,199-song list whose songs after the one playing are all on the card already: what one
// pass of the run loop costs in it (twice a second during a fetch, once a second otherwise).
func BenchmarkPlanAllWholeAhead9199(b *testing.B) {
	e := newEnvB(b)
	e.s.caps.Ahead, e.s.aheadFile = true, filepath.Join(b.TempDir(), "ahead")
	r := &streamRun{s: e.s, songs: songs(9199, "flac"), playSeq: 7}
	for i := range r.songs {
		p := filepath.Join(e.root, fmt.Sprintf("%d.flac", i))
		os.WriteFile(p, nil, 0o644)
		r.jobs = append(r.jobs, job{path: p, fetch: true, cached: true})
	}
	os.WriteFile(e.s.aheadFile, []byte("#Q2AHEAD/1\tseq=7\tn=2\n"), 0o644)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.plan(0)
	}
}

// A long list's queue file names queueAhead songs past the one playing, not all of it (the firmware
// reads 256 KB, about 2,300 cache paths, of a file a 9,199-song Shuffle made about 1 MB), and grows as
// the list plays, its lines before the same.
func TestALongListsQueueFileIsAWindowThatGrows(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.start(songs(9199, "flac"), 0)
	lines := func() []string {
		_, body := e.queue(t)
		return strings.Split(strings.TrimSuffix(body, "\n"), "\n")[1:] // after #EXTM3U
	}
	first := lines()
	if len(first) != queueAhead+1 || first[0] != e.stream(0, "flac") || first[queueAhead] != e.stream(queueAhead, "flac") {
		t.Fatalf("%d lines, first %q", len(first), first[0])
	}
	os.WriteFile(e.playing, []byte(e.stream(600, "flac")+"\n"), 0o644)
	until(t, "the file grown", func() bool { return len(lines()) == 600+queueAhead+1 })
	if got := lines(); !slices.Equal(got[:len(first)], first) {
		t.Fatal("the lines before changed")
	}
}
