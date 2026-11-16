package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// With "album", an album's page has Shuffle and an about row (the album's artist; its year, song
// count and running time) after Play, and each song says its length before its status.
func TestAlbumPageWithAlbumWord(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3}, "album", true)
	sameRows(t, "page", d.do("album", "album:al1"),
		"action toggle Add to Sync |  | album:al1",
		"action play Play |  | album:al1 0",
		"action shuffle Shuffle |  | album:al1",
		"info about Artist | 2020 · 3 songs · 5 min | ",
		"action play Song 0 | Artist · 1:40 | song:al1.0 album:al1 0",
		"action play Song 1 | Artist · 1:40 | song:al1.1 album:al1 1",
		"action play Song 2 | Artist · 1:40 | song:al1.2 album:al1 2")
	// a compilation: the album's own artist, each song its own
	d = newTestDaemon(t, &nav{albums: 1, songsPer: 2, various: true}, "album", true)
	sameRows(t, "compilation", d.do("album", "album:al0"),
		"action toggle Add to Sync |  | album:al0",
		"action play Play |  | album:al0 0",
		"action shuffle Shuffle |  | album:al0",
		"info about Various Artists | 2020 · 2 songs · 3 min | ",
		"action play Song 0 | Artist 0 · 1:40 | song:al0.0 album:al0 0",
		"action play Song 1 | Artist 1 · 1:40 | song:al0.1 album:al0 1")
}

// Firmware without "album" gets the page it always had, and an artist's page is not an album's.
func TestAlbumPageWithoutAlbumWord(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "artist", true)
	sameRows(t, "page", d.do("album", "album:al0"),
		"action toggle Add to Sync |  | album:al0",
		"action play Play |  | album:al0 0",
		"action play Song 0 | Artist | song:al0.0 album:al0 0",
		"action play Song 1 | Artist | song:al0.1 album:al0 1")
	d = newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "album", true)
	if r := d.do("artist", "artist:ar1"); strings.Contains(strings.Join(rowsOf(r), "\n"), "info about") {
		t.Fatalf("an artist's page with an album's rows: %q", rowsOf(r))
	}
}

// Shuffle on an album's page plays every song of the album, each once.
func TestShuffleAlbum(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 4}, "playlist album", true)
	r := d.do("shuffle", "album:al1")
	if !strings.HasPrefix(r.Message, "Playing Song ") {
		t.Fatalf("shuffle: %+v", r)
	}
	d.waitQueued(t, 4)
	m, _ := filepath.Glob(filepath.Join(d.card, ".subsonic", "stream", "queue-*.m3u"))
	if b, _ := os.ReadFile(m[0]); strings.Count(string(b), "[al1.") != 4 {
		t.Fatalf("the queue is not the album's songs:\n%s", b)
	}
	songs := []subsonic.Song{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	ids := func(s []subsonic.Song) []string {
		var o []string
		for _, x := range s {
			o = append(o, x.ID)
		}
		return o
	}
	out := ids(shuffled(songs))
	slices.Sort(out)
	if in := strings.Join(ids(songs), ""); in != "abcde" || strings.Join(out, "") != "abcde" {
		t.Fatalf("shuffled changed its input (%q) or is not a permutation of it (%q)", in, out)
	}
}

func TestAlbumSpan(t *testing.T) {
	for sec, want := range map[int]string{0: "", -5: "", 20: "1 min", 89: "1 min", 90: "2 min", 2280: "38 min",
		3599: "1h", 3600: "1h", 4320: "1h12m", 7290: "2h2m"} {
		if got := span(sec); got != want {
			t.Errorf("span(%d) = %q, want %q", sec, got, want)
		}
	}
}
