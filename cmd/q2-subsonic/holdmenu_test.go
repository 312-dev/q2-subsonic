package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
)

// albumSongRow is song j's row on album al0's page.
func albumSongRow(t *testing.T, d *tdaemon, j int) q2spot.Row {
	t.Helper()
	page := d.do("album", "album:al0")
	row, ok := find(page, "Song "+string(rune('0'+j)))
	if !ok {
		t.Fatalf("no song %d in %q", j, titles(page))
	}
	return row
}

// heldLines is /tmp/q2-subsonic.queue's lines, "" for none.
func (d *tdaemon) heldLines() []string {
	b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-subsonic.queue"))
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// A song row's hold op is "hold" on firmware with "holdmenu", else "toggle" as before; an album's
// row keeps "toggle" either way.
func TestSongRowsHoldOpFollowsTheWord(t *testing.T) {
	for _, c := range []struct{ caps, want string }{{"playlist", "toggle"}, {"playlist holdmenu", "hold"}} {
		d := newTestDaemon(t, &nav{albums: 1, songsPer: 3}, c.caps, true)
		if row := albumSongRow(t, d, 1); row.Position != c.want || !strings.HasPrefix(row.Context, "song:al0.1 ") {
			t.Errorf("%s: song row %+v", c.caps, row)
		}
		if row, _ := find(d.do("albums"), "Album 000"); row.Position != "toggle" {
			t.Errorf("%s: album row %+v", c.caps, row)
		}
	}
}

// Without the word the firmware sends none of the ops, and the app takes none.
func TestHoldOpsNeedTheWord(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3}, "playlist", true)
	ctx := albumSongRow(t, d, 1).Context
	for _, op := range []string{"hold", "next", "last"} {
		if r := d.do(op, ctx); r.Status != q2spot.Unsupported {
			t.Errorf("%s: %+v", op, r)
		}
	}
	if lines := d.heldLines(); lines[0] != "" {
		t.Errorf("queue %q", lines)
	}
}

// The menu's rows are the firmware song menu's, in its order and words: Play Next, Play Last, then
// Sync Offline, which turns to Stop Syncing Offline once chosen and is left out under Entire Library.
func TestHoldMenuRows(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3}, "playlist holdmenu", true)
	d.bg.Wait()
	ctx := albumSongRow(t, d, 1).Context
	r := d.do("hold", ctx)
	if got := strings.Join(titles(r), "|"); got != "Play Next|Play Last|Sync Offline" || r.Status != "" {
		t.Fatalf("rows %q %+v", got, r)
	}
	for i, want := range []q2spot.Row{act("next", "Play Next", ctx), act("last", "Play Last", ctx), act("toggle", "Sync Offline", "song:al0.1")} {
		if r.Rows[i] != want {
			t.Errorf("row %d %+v, want %+v", i, r.Rows[i], want)
		}
	}
	if t2 := d.do(r.Rows[2].URI, r.Rows[2].Context); t2.Message != "Syncing offline · 1 song" {
		t.Fatalf("toggle %q", t2.Message)
	}
	if got := titles(d.do("hold", ctx)); got[2] != "Stop Syncing Offline" {
		t.Errorf("chosen: %q", got)
	}
	d.do("toggle", "song:al0.1")
	d.do("toggle", "library:all")
	if got := strings.Join(titles(d.do("hold", ctx)), "|"); got != "Play Next|Play Last" {
		t.Errorf("under Entire Library: %q", got)
	}
	if r := d.do("hold", "album:al0"); r.Status != q2spot.Bad {
		t.Errorf("an album's context: %+v", r)
	}
}

// With the Stream Cache Off a song not on the card has nowhere to be kept: no Play Next or Play Last.
func TestHoldMenuWithoutTheCache(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3}, "playlist holdmenu", true)
	d.bg.Wait()
	for d.settings().cacheBytes() != 0 {
		d.do("cachesz")
	}
	if got := strings.Join(titles(d.do("hold", albumSongRow(t, d, 1).Context)), "|"); got != "Sync Offline" {
		t.Fatalf("rows %q", got)
	}
}

// Play Next fetches the song into the Stream Cache and names it in .queue once whole; Play Last on
// the same song, kept now, names it at once with the next seq.
func TestPlayNextAndLastNameTheSongOnceWhole(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 3}, "playlist holdmenu", true)
	d.bg.Wait()
	ctx := albumSongRow(t, d, 2).Context
	if r := d.do("next", ctx); r.Message != "Playing Next once downloaded" || r.Status != "" {
		t.Fatalf("next %+v", r)
	}
	var path string
	eventually(t, "the queue line", func() bool {
		f := strings.Fields(d.heldLines()[0])
		if len(f) >= 3 && f[1] == "next" {
			path = strings.SplitN(d.heldLines()[0], " ", 3)[2]
		}
		return path != ""
	})
	if !d.stream.inCache(path) || streamID(path) != "al0.2" {
		t.Fatalf("path %q is not the song's cache copy", path)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) != 1000 || exists(path+".part") {
		t.Fatalf("not whole: %d bytes, %v", len(b), err)
	}
	if r := d.do("last", ctx); r.Message != "" || r.Status != "" {
		t.Fatalf("last %+v", r)
	}
	lines := d.heldLines()
	if len(lines) != 2 || !strings.HasSuffix(lines[1], " last "+path) {
		t.Fatalf("lines %q", lines)
	}
	if a, b := strings.Fields(lines[0])[0], strings.Fields(lines[1])[0]; a == b {
		t.Errorf("one seq twice: %q", lines)
	}
}

// A synced song is named by its card path, with no fetch.
func TestPlayLastOfASyncedSong(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3}
	d := newTestDaemon(t, n, "playlist holdmenu", true)
	d.bg.Wait()
	ctx := albumSongRow(t, d, 0).Context
	synced := d.cacheFile(t, "al0.0", 10) // a whole copy on the card
	fetched := n.reqs("stream") + n.reqs("download")
	if r := d.do("last", ctx); r.Message != "" || r.Status != "" {
		t.Fatalf("last %+v", r)
	}
	if lines := d.heldLines(); len(lines) != 1 || !strings.HasSuffix(lines[0], " last "+synced) {
		t.Fatalf("lines %q", lines)
	}
	if got := n.reqs("stream") + n.reqs("download"); got != fetched {
		t.Errorf("fetched %d", got-fetched)
	}
}

// .queue keeps its latest queueKeep lines, each seq above the one before.
func TestQueueKeepsItsLatestLines(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "playlist holdmenu", true)
	for i := 0; i < queueKeep+3; i++ {
		if err := d.queueLine("last", filepath.Join(d.card, "Subsonic", "a", string(rune('a'+i))+".flac")); err != nil {
			t.Fatal(err)
		}
	}
	lines := d.heldLines()
	if len(lines) != queueKeep || !strings.HasSuffix(lines[0], "/d.flac") || !strings.HasSuffix(lines[queueKeep-1], "/k.flac") {
		t.Fatalf("lines %q", lines)
	}
	if err := d.queueLine("next", "/mnt/mmc/a\nb.flac"); err == nil {
		t.Error("a path with a line end was written")
	}
}

// The songs Play Next and Play Last named are kept from a trim, as the list playing is.
func TestCacheHoldsQueuedSongs(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.cacheOn(1, 1<<40)
	var f []string
	for i := 0; i < 4; i++ {
		f = append(f, e.kept(i, "flac", "original"))
		put(t, f[i], 100, 10-i) // song 3 is the oldest
	}
	e.cacheOn(100, 1<<40)
	e.s.holdQueued(f[3])
	e.s.protect([]string{f[2]}) // a new list's protection leaves the queued songs held
	e.s.trim(context.Background(), 100, 0)
	if !exists(f[3]) || !exists(f[2]) || exists(f[0]) || exists(f[1]) {
		t.Errorf("kept %v %v %v %v", exists(f[0]), exists(f[1]), exists(f[2]), exists(f[3]))
	}
}
