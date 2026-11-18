package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// restored is a song's file as the stream cache names it, where the firmware's snapshot has it.
func (d *tdaemon) restored(j int) string {
	return filepath.Join(d.stream.dir(), "acct", "original", "s", fmt.Sprintf("Song %d [al0.%d].flac", j, j))
}

// resumeAsk writes the firmware's .resume request for album 0's songs, those in onCard already on the
// card, and has the app read it, as its tick does.
func (d *tdaemon) resumeAsk(t *testing.T, head string, songs int, onCard ...int) {
	t.Helper()
	for _, j := range onCard {
		os.MkdirAll(filepath.Dir(d.restored(j)), 0o755)
		if err := os.WriteFile(d.restored(j), make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	req := head + "\n"
	for j := range songs {
		req += d.restored(j) + "\n"
	}
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.resume"), []byte(req), 0o644)
	d.tick()
}

// A queue of ten streamed songs restored after a restart, with the 4th and 5th on the card and the
// 4th the one restored: the app plays the list on from it, as a queue answering the request's seq,
// the song restored first and kept where it is; the songs after it download ahead, none before it is
// fetched, and no play is reported while the firmware has it paused (.playing names nothing).
func TestResumePlaysOnFromTheSongRestoredPaused(t *testing.T) {
	n := &nav{albums: 1, songsPer: 10}
	d := newTestDaemon(t, n, "playlist", true)
	d.client()
	d.resumeAsk(t, "7 3 42 paused", 10, 3, 4)
	eventually(t, "the request answered", func() bool { return d.stateKey("resumeack") == "7 0" })
	d.waitQueued(t, 7)
	q := d.queueLines()
	if q[0] != d.restored(3) || q[1] != d.restored(4) {
		t.Fatalf("the queue starts %q, not the songs restored", q[:2])
	}
	for i, l := range q {
		if streamID(l) != fmt.Sprintf("al0.%d", i+3) {
			t.Fatalf("queue line %d is %q", i, l)
		}
	}
	if b, _ := os.ReadFile(d.stream.playFile); !strings.HasPrefix(string(b), "7 ") {
		t.Fatalf(".play %q does not answer the request", b)
	}
	eventually(t, "the song after the two on the card fetched", func() bool { return n.fetches("al0.5") == 1 })
	for _, id := range []string{"al0.0", "al0.1", "al0.2", "al0.3", "al0.4"} {
		if got := n.fetches(id); got != 0 {
			t.Fatalf("%s fetched %d times", id, got)
		}
	}
	if got := n.reqs("scrobble"); got != 0 {
		t.Fatalf("%d plays reported while the song restored waits paused", got)
	}
	os.WriteFile(d.stream.playingFile, []byte(q[0]+"\n"), 0o644) // the user's Play
	eventually(t, "the play reported", func() bool { return n.reqs("scrobble") == 1 })
	if got := n.query("scrobble").Get("id"); got != "al0.3" {
		t.Fatalf("reported %q", got)
	}
}

// A request is answered once; one whose songs are none of the server's is answered 1, signed out 2.
func TestResumeDeclines(t *testing.T) {
	n := &nav{albums: 1, songsPer: 4}
	d := newTestDaemon(t, n, "playlist", true)
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.resume"), []byte("8 0 0 paused\n/mnt/mmc/Music/a.flac\n"), 0o644)
	d.tick()
	if got := d.stateKey("resumeack"); got != "8 1" {
		t.Fatalf("not ours: resumeack=%q", got)
	}
	out := newTestDaemon(t, n, "playlist", false)
	out.resumeAsk(t, "9 0 0 paused", 4, 0)
	if got := out.stateKey("resumeack"); got != "9 2" {
		t.Fatalf("signed out: resumeack=%q", got)
	}
}
