package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/library"
)

// askLyrics writes the firmware's .lrcask request and has the app read it, as its tick does.
func (d *tdaemon) askLyrics(line string) {
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.lrcask"), []byte(line+"\n"), 0o644)
	d.lyricsTick()
}

// song puts a streamed song's file on the card, as a list leaves it, and returns its path.
func (d *tdaemon) song(t *testing.T, id string) string {
	t.Helper()
	p := d.streamed(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func lrcOf(t *testing.T, song string) string {
	t.Helper()
	b, err := os.ReadFile(strings.TrimSuffix(song, filepath.Ext(song)) + ".lrc")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A server with OpenSubsonic's songLyrics: the song's synced lines go beside its file as LRC, to the
// hundredth, the server's offset applied as LRC's own, and the request is answered. A second ask for
// the song finds the file and asks the server nothing.
func TestLyricsSynced(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, lyricExt: true, lyricOffset: 500,
		synced: map[string][]string{"al0.1": {"1500 line one", "3000 line two", "65250 line three"}}}
	d := newTestDaemon(t, n, "playlist", true)
	p := d.song(t, "al0.1")
	d.askLyrics("5 " + p)
	eventually(t, "the request answered", func() bool { return d.stateKey("lyricsack") == "5 0" })
	if q := n.query("getLyricsBySongId"); q.Get("id") != "al0.1" {
		t.Fatalf("query %v", q)
	}
	want := "[00:01.00]line one\n[00:02.50]line two\n[01:04.75]line three\n"
	if got := lrcOf(t, p); got != want {
		t.Fatalf("lrc %q, want %q", got, want)
	}
	d.askLyrics("6 " + p)
	eventually(t, "the second request answered", func() bool { return d.stateKey("lyricsack") == "6 0" })
	if n.reqs("getLyricsBySongId") != 1 || n.reqs("getOpenSubsonicExtensions") != 1 {
		t.Fatalf("asked the server again: %v", n.reqN)
	}
}

// A server without songLyrics: getLyrics with the song's artist and title, its text a plain line a
// line.
func TestLyricsPlain(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, plain: map[string]string{"Song 2": "line one\r\nline two\n"}}
	d := newTestDaemon(t, n, "playlist", true)
	p := d.song(t, "al0.2")
	d.askLyrics("7 " + p)
	eventually(t, "the request answered", func() bool { return d.stateKey("lyricsack") == "7 0" })
	if q := n.query("getLyrics"); q.Get("artist") != "Artist" || q.Get("title") != "Song 2" {
		t.Fatalf("query %v", q)
	}
	if got := lrcOf(t, p); got != "line one\nline two\n" {
		t.Fatalf("lrc %q", got)
	}
}

// A song the server has no lyrics for is answered as none, and remembered, by a later start of the
// app too: the server is not asked about it again.
func TestLyricsNoneRemembered(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, lyricExt: true}
	d := newTestDaemon(t, n, "playlist", true)
	p := d.song(t, "al0.0")
	d.askLyrics("3 " + p)
	eventually(t, "the request answered", func() bool { return d.stateKey("lyricsack") == "3 2" })
	d.askLyrics("4 " + p)
	eventually(t, "the second request answered", func() bool { return d.stateKey("lyricsack") == "4 2" })
	d.lyr.none = nil // as a later start has it: read from the card
	d.askLyrics("5 " + p)
	eventually(t, "the next start's request answered", func() bool { return d.stateKey("lyricsack") == "5 2" })
	if got := n.reqs("getLyricsBySongId"); got != 1 {
		t.Fatalf("getLyricsBySongId asked %d times", got)
	}
	if _, err := os.Stat(strings.TrimSuffix(p, ".flac") + ".lrc"); err == nil {
		t.Fatal("a .lrc for no lyrics")
	}
}

// A server with neither call is asked once in the app's run: every song after is none.
func TestLyricsServerWithout(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3}
	d := newTestDaemon(t, n, "playlist", true)
	d.askLyrics("1 " + d.song(t, "al0.0"))
	eventually(t, "the request answered", func() bool { return d.stateKey("lyricsack") == "1 2" })
	d.askLyrics("2 " + d.song(t, "al0.1"))
	eventually(t, "the second request answered", func() bool { return d.stateKey("lyricsack") == "2 2" })
	if n.reqs("getLyrics") != 1 || n.reqs("getOpenSubsonicExtensions") != 1 {
		t.Fatalf("asked %v", n.reqN)
	}
}

// A song that is not the server's is not the app's to answer; a server out of reach is a failure,
// not remembered: the next ask goes to the server.
func TestLyricsNotOursAndLost(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, lyricExt: true, synced: map[string][]string{"al0.0": {"0 line one"}}}
	d := newTestDaemon(t, n, "playlist", true)
	d.askLyrics("1 " + filepath.Join(d.card, "Music", "a.flac"))
	eventually(t, "a card song's request answered", func() bool { return d.stateKey("lyricsack") == "1 1" })
	p := d.song(t, "al0.0")
	defer func(was time.Duration) { lyricsIn = was }(lyricsIn)
	lyricsIn = 200 * time.Millisecond
	n.stall = true
	d.askLyrics("2 " + p)
	eventually(t, "the lost server's answer", func() bool { return d.stateKey("lyricsack") == "2 3" })
	n.stall = false
	d.askLyrics("3 " + p)
	eventually(t, "the request answered", func() bool { return d.stateKey("lyricsack") == "3 0" })
}

// The .lrc goes with its song, and the stream cache never takes it for a song.
func TestLyricsFileFollowsSong(t *testing.T) {
	card := t.TempDir()
	song := filepath.Join(card, ".subsonic", "stream", "abcd", "original", "0", "song [s1].flac")
	lrc := strings.TrimSuffix(song, ".flac") + ".lrc"
	os.MkdirAll(filepath.Dir(song), 0o755)
	os.WriteFile(song, []byte("x"), 0o644)
	os.WriteFile(lrc, []byte("line one\n"), 0o644)
	s := &streamer{root: card, acct: func() string { return "abcd" }}
	if c := s.copies(); c["s1"] != song {
		t.Fatalf("copies %v", c)
	}
	for _, f := range s.cacheFiles() {
		if f.path == lrc {
			t.Fatal("the .lrc is a cache file")
		}
	}
	if err := library.Remove(card, song); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lrc); err == nil {
		t.Fatal("the .lrc stayed")
	}
}

// The request read at the app's start was the run before's: not answered again.
func TestLyricsRequestAtStart(t *testing.T) {
	n := &nav{albums: 1, songsPer: 3, lyricExt: true}
	card, tmp := t.TempDir(), ""
	d := newTestDaemonOn(t, card, n, "playlist", true)
	tmp = d.tmp
	p := d.song(t, "al0.0")
	os.WriteFile(filepath.Join(tmp, "q2-subsonic.lrcask"), []byte("9 "+p+"\n"), 0o644)
	d.lyricsInit()
	d.lyricsTick()
	d.bg.Wait()
	if d.stateKey("lyricsack") != "" || n.reqs("getLyricsBySongId") != 0 {
		t.Fatalf("answered %q", d.stateKey("lyricsack"))
	}
}
