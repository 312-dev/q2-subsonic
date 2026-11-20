package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// offlineAsk writes the firmware's .offline request and has the app read it, as its tick does, then
// waits for the answer: the words after the seq of offlineack=.
func (d *tdaemon) offlineAsk(t *testing.T, line string) string {
	t.Helper()
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.offline"), []byte(line+"\n"), 0o644)
	d.offlineTick()
	d.bg.Wait()
	seq, _, _ := strings.Cut(line, " ")
	got, ok := strings.CutPrefix(d.stateKey("offlineack"), seq+" ")
	if !ok {
		t.Fatalf("%q: offlineack=%q", line, d.stateKey("offlineack"))
	}
	return got
}

func (d *tdaemon) offlineList(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(d.dir, "offline.txt"))
	return string(b)
}

// The firmware's Library asks to sync an item offline or stop: the item goes in or out of the
// selection, the answer is the toast's words, and offline.txt, which names the hold menu's row,
// follows. The same request read again is not answered again.
func TestOfflineRequestAddsAndRemoves(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3}, "", true)
	if got := d.offlineAsk(t, "1 add album al0"); got != "Syncing offline · 3 songs" {
		t.Fatalf("add: %q", got)
	}
	if got := d.offlineList(t); got != "album\tal0\tAlbum 000\n" {
		t.Fatalf("offline.txt %q", got)
	}
	if got := d.offlineAsk(t, "2 add album al0"); got != "Already syncing offline" {
		t.Fatalf("again: %q", got)
	}
	d.state.setOfflineAck("x")
	d.offlineTick() // the same request: not answered twice
	d.bg.Wait()
	if v := d.stateKey("offlineack"); v != "x" {
		t.Fatalf("answered twice: %q", v)
	}
	if got := d.offlineAsk(t, "3 remove album al0"); got != "Stopped syncing offline" {
		t.Fatalf("remove: %q", got)
	}
	if got := d.offlineList(t); got != "" {
		t.Fatalf("offline.txt after removing %q", got)
	}
	if got := d.offlineAsk(t, "4 remove song al1.0"); got != "Not syncing offline" {
		t.Fatalf("remove what isn't chosen: %q", got)
	}
}

// The Library knows its artists by name: the app finds the server's artist of that name, any case.
func TestOfflineArtistByName(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 1}, "", true)
	if got := d.offlineAsk(t, "1 add artistname ARTIST"); !strings.HasPrefix(got, "Syncing offline · ") {
		t.Fatalf("add: %q", got)
	}
	if got := d.offlineList(t); !strings.HasPrefix(got, "artist\tar1\tArtist\n") {
		t.Fatalf("offline.txt %q", got)
	}
	if got := d.offlineAsk(t, "2 add artistname Nobody"); got != "Not on the server" {
		t.Fatalf("unknown artist: %q", got)
	}
}

// With Entire Library chosen an item is already offline through it; signed out nothing is asked.
func TestOfflineEntireLibraryAndSignedOut(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	d.do("toggle", "library:all")
	if got := d.offlineList(t); got != "library\tall\tEntire Library\n" {
		t.Fatalf("offline.txt %q", got)
	}
	if got := d.offlineAsk(t, "1 add album al0"); got != "Offline via Entire Library" {
		t.Fatalf("add under Entire Library: %q", got)
	}
	out := newTestDaemon(t, &nav{}, "", false)
	if got := out.offlineAsk(t, "1 add album al0"); got != "Sign in to Subsonic first" {
		t.Fatalf("signed out: %q", got)
	}
	if got := out.offlineAsk(t, "2 add folder x"); got != "Sign in to Subsonic first" {
		t.Fatalf("signed out, bad kind: %q", got)
	}
	if got := d.offlineAsk(t, "2 add folder x"); got != "Can't sync that offline" {
		t.Fatalf("bad kind: %q", got)
	}
}

// A request there before the app started is taken as answered: the firmware writes one only once
// the app's socket is there. offline.txt is written at the start from the selection.
func TestOfflineInit(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	d.do("toggle", "album:al0")
	os.Remove(filepath.Join(d.dir, "offline.txt"))
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.offline"), []byte("9 add album al0\n"), 0o644)
	d.offlineInit()
	d.offlineTick()
	d.bg.Wait()
	if v := d.stateKey("offlineack"); v != "" {
		t.Fatalf("answered an old request: %q", v)
	}
	if got := d.offlineList(t); got != "album\tal0\tAlbum 000\n" {
		t.Fatalf("offline.txt at start %q", got)
	}
}
