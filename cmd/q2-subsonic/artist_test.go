package main

import (
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
)

// rowsOf is each row as "kind uri title | subtitle | context".
func rowsOf(r q2spot.Reply) []string {
	var out []string
	for _, row := range r.Rows {
		out = append(out, row.Kind+" "+row.URI+" "+row.Title+" | "+row.Subtitle+" | "+row.Context)
	}
	return out
}

func sameRows(t *testing.T, what string, r q2spot.Reply, want ...string) {
	t.Helper()
	if got := rowsOf(r); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s:\n%s\nwant:\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// With "artist", an artist's page says its counts and genre, offers Singles & EPs, Similar Artists
// and All Songs, and lists only its albums; release types and genres come from getArtist.
func TestArtistPageWithArtistWord(t *testing.T) {
	n := &nav{albums: 5, songsPer: 3, explore: true}
	d := newTestDaemon(t, n, "artist", true)
	sameRows(t, "page", d.do("artist", "artist:ar1"),
		"action toggle Add to Sync |  | artist:ar1",
		"action play Play |  | artist:ar1 0",
		"action shuffle Shuffle |  | artist:ar1",
		"action radio Radio |  | artist:ar1",
		"info about 3 albums · 2 singles · 15 songs | Electronic | ",
		"list top Top Songs |  | artist:ar1",
		"list singles Singles & EPs | 2 releases | artist:ar1",
		"list similar Similar Artists | 1 artist | artist:ar1",
		"list songs All Songs | 15 songs | artist:ar1",
		"list album Album 000 | 2020 | album:al0",
		"list album Album 003 | 2020 | album:al3",
		"list album Album 004 | 2020 | album:al4")
	if q := n.query("getArtistInfo2"); q.Get("id") != "ar1" || q.Get("count") != "20" {
		t.Fatalf("getArtistInfo2 %v", q)
	}
	sameRows(t, "singles", d.do("singles", "artist:ar1"),
		"list album Album 001 | 2020 | album:al1", "list album Album 002 | 2020 | album:al2")
	sim := d.do("similar", "artist:ar1")
	sameRows(t, "similar", sim, "list artist Duke Dumont | 3 albums | artist:ar2")
	if sim.Rows[0].Position != "toggle" {
		t.Fatalf("a similar artist is an artist row: %+v", sim.Rows[0])
	}
}

// Firmware without "artist" gets the page it always had, whatever the server says.
func TestArtistPageWithoutArtistWord(t *testing.T) {
	n := &nav{albums: 3, songsPer: 3, explore: true}
	d := newTestDaemon(t, n, "", true)
	sameRows(t, "page", d.do("artist", "artist:ar1"),
		"action toggle Add to Sync |  | artist:ar1",
		"action play Play |  | artist:ar1 0",
		"action shuffle Shuffle |  | artist:ar1",
		"action radio Radio |  | artist:ar1",
		"list top Top Songs |  | artist:ar1",
		"list album Album 000 | 2020 | album:al0",
		"list album Album 001 | 2020 | album:al1",
		"list album Album 002 | 2020 | album:al2")
	if n.reqs("getArtistInfo2") != 0 {
		t.Fatal("getArtistInfo2 asked without the word")
	}
	r := d.do("top", "artist:ar1")
	if r.Rows[0].Subtitle != "Artist" || r.Rows[0].Context != "song:al0.0 top:ar1 0" {
		t.Fatalf("top song %+v", r.Rows[0])
	}
}

// A server with no release types, genres or artist info (plain Subsonic, or no OpenSubsonic
// fields): every release is an album, no genre, no Similar Artists; one without Top Songs has no
// such row.
func TestArtistPageWithoutServerData(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3}, "artist", true)
	sameRows(t, "page", d.do("artist", "artist:ar1"),
		"action toggle Add to Sync |  | artist:ar1",
		"action play Play |  | artist:ar1 0",
		"action shuffle Shuffle |  | artist:ar1",
		"action radio Radio |  | artist:ar1",
		"info about 2 albums · 6 songs |  | ",
		"list top Top Songs |  | artist:ar1",
		"list songs All Songs | 6 songs | artist:ar1",
		"list album Album 000 | 2020 | album:al0",
		"list album Album 001 | 2020 | album:al1")
	d = newTestDaemon(t, &nav{albums: 2, songsPer: 3, noLastfm: true}, "artist", true)
	if _, ok := find(d.do("artist", "artist:ar1"), "Top Songs"); ok {
		t.Fatal("Top Songs on a server without it")
	}
	d = newTestDaemon(t, &nav{albums: 2, songsPer: 3, empty: true}, "artist", true)
	if _, ok := find(d.do("artist", "artist:ar1"), "Top Songs"); ok {
		t.Fatal("Top Songs with none")
	}
	d = newTestDaemon(t, &nav{albums: 2, songsPer: 3, noAlbums: true}, "artist", true)
	sameRows(t, "no albums", d.do("artist", "artist:ar1"), "action toggle Add to Sync |  | artist:ar1", "info  No albums |  | ")
}

// Artist info that doesn't come in time leaves Similar Artists out, and the page is not held up.
func TestArtistPageDoesNotWaitForSimilarArtists(t *testing.T) {
	was := artistMoreIn
	artistMoreIn = 200 * time.Millisecond
	t.Cleanup(func() { artistMoreIn = was })
	n := &nav{albums: 3, songsPer: 3, explore: true, holdInfo: make(chan struct{})}
	defer close(n.holdInfo)
	d := newTestDaemon(t, n, "artist", true)
	start := time.Now()
	r := d.do("artist", "artist:ar1")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the page took %v", took)
	}
	if _, ok := find(r, "Similar Artists"); ok {
		t.Fatal("Similar Artists without an answer")
	}
	if _, ok := find(r, "Singles & EPs"); !ok {
		t.Fatalf("the rest of the page is there: %q", rowsOf(r))
	}
}

// Top Songs and All Songs, with "artist": each song's album and length before its status, and its
// album id after its play context; a press plays on through the list.
func TestArtistSongsWithArtistWord(t *testing.T) {
	n := &nav{albums: 2, songsPer: 3}
	d := newTestDaemon(t, n, "playlist artist", true)
	top := d.do("top", "artist:ar1")
	if row := top.Rows[1]; row.Subtitle != "Album 000 · 1:40" || row.Context != "song:al0.1 top:ar1 1 album:al0" {
		t.Fatalf("top song %+v", row)
	}
	if r := d.do("play", top.Rows[1].Context); r.Message != "Playing Song 1" {
		t.Fatalf("play: %+v", r)
	}
	d.waitQueued(t, 2)
	all := d.do("songs", "artist:ar1")
	if len(all.Rows) != 6 {
		t.Fatalf("all songs %q", rowsOf(all))
	}
	if row := all.Rows[4]; row.Title != "Song 1" || row.Subtitle != "Album 001 · 1:40" || row.Context != "song:al1.1 songs:ar1 4 album:al1" {
		t.Fatalf("all songs' row %+v", row)
	}
	if r := d.do("play", all.Rows[4].Context); r.Message != "Playing Song 1" {
		t.Fatalf("play: %+v", r)
	}
	d.waitQueued(t, 2) // the list from row 4 on: two songs
}

func TestSongLength(t *testing.T) {
	for sec, want := range map[int]string{0: "", -1: "", 5: "0:05", 100: "1:40", 3599: "59:59", 3600: "1:00:00", 3723: "1:02:03"} {
		if got := length(sec); got != want {
			t.Errorf("length(%d) = %q, want %q", sec, got, want)
		}
	}
}
