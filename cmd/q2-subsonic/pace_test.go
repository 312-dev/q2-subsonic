package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/catalogue"
	"github.com/andsuor/q2-subsonic/internal/fakesonic"
)

// Pacing (Q2 Pod's docs/internals.md, "Bluetooth coexistence pacing"): while q2guard's /tmp/q2-bt-coex says paced=1
// (A2DP streaming with the Q2's Wi-Fi on 2.4 GHz), the app holds its background downloads to its rate and pauses its
// rounds of covers; the song playing is held only while it is well ahead of where it plays.

const paced = "paced=1 rate=204800 freq=2412\n"

// paceRig is a catRig whose pace gate reads its file at every look and keeps the time it held downloads, on a clock
// of its own that each hold moves on (no test waits it out), with which song had landed by then.
type paceRig struct {
	*catRig
	mu     sync.Mutex
	now    time.Time
	held   map[int]time.Duration // by how many songs of the stream had landed when it was held
	landed int
}

func newPaceRig(t *testing.T, caps string, songs []fakesonic.Song) *paceRig {
	r := &paceRig{catRig: newCatRig(t, caps, songs), now: time.Unix(1795464061, 0), held: map[int]time.Duration{}}
	r.pace.Every = 0
	r.pace.Now = func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }
	r.pace.Sleep = func(ctx context.Context, d time.Duration) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.now = r.now.Add(d)
		r.held[r.landed] += d
		return ctx.Err()
	}
	r.stream.landed = func() { r.mu.Lock(); r.landed++; r.mu.Unlock() }
	return r
}

func (r *paceRig) say(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.tmp, "q2-bt-coex"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Covers are a round's work, nothing anyone waits for: while paced none is asked for, even on the charger (where a
// round runs whatever else the app does); unpaced, the round goes on.
func TestCoversWaitWhilePaced(t *testing.T) {
	r := newPaceRig(t, "catalogue", fakesonic.Library(30)) // ten albums
	r.f.Mu.Lock()
	r.f.Covers = map[string][]byte{"al0": testJPEG(t, 160), "al1": testJPEG(t, 160)}
	r.f.Mu.Unlock()
	batteryStatus = filepath.Join(r.tmp, "battery")
	t.Cleanup(func() { batteryStatus = "/sys/class/power_supply/battery/status" })
	os.WriteFile(batteryStatus, []byte("Charging\n"), 0o644)
	r.check(t) // the first copy and the catalogue
	r.say(t, paced)
	r.check(t)
	if n := r.calls("getCoverArt"); n != 0 {
		t.Fatal("covers while paced:", n)
	}
	r.say(t, "paced=0 rate=0 freq=5180\n")
	r.check(t)
	if n := r.calls("getCoverArt"); n != 10 {
		t.Fatal("covers once unpaced:", n)
	}
}

// A queue from the Library while paced: the song pressed, which nothing has played yet (so it is behind where it
// will play), arrives unheld; the two fetched ahead of it are held to 200 KB/s, about four seconds each of their
// megabyte past the first second's worth.
func TestAheadIsHeldAndTheSongPlayingBehindIsNot(t *testing.T) {
	r := newPaceRig(t, "playlist catalogue", fakesonic.Library(3))
	r.check(t)
	r.say(t, paced)
	row := func(sid string) string { return catalogue.FileURL(r.card, "subsonic", "al0", sid, "flac") }
	req := strings.Join([]string{"7 0", row("s1"), row("s2"), row("s3")}, "\n") + "\n"
	os.WriteFile(r.catplayPath(), []byte(req), 0o644)
	r.slTick()
	r.bg.Wait()
	eventually(t, "three songs landed", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.landed == 3 })
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held[0] != 0 {
		t.Fatal("the song playing, behind, was held", r.held[0])
	}
	for _, i := range []int{1, 2} {
		if r.held[i] < 3500*time.Millisecond || r.held[i] > 5*time.Second {
			t.Fatal(fmt.Sprintf("song %d ahead held %v", i+1, r.held[i]))
		}
	}
}

// Unpaced (5 GHz, or no stream), nothing is held.
func TestNothingIsHeldUnpaced(t *testing.T) {
	r := newPaceRig(t, "playlist catalogue", fakesonic.Library(3))
	r.check(t)
	r.say(t, "paced=0 rate=0 freq=5180\n")
	row := func(sid string) string { return catalogue.FileURL(r.card, "subsonic", "al0", sid, "flac") }
	os.WriteFile(r.catplayPath(), []byte("8 0\n"+row("s1")+"\n"+row("s2")+"\n"), 0o644)
	r.slTick()
	r.bg.Wait()
	eventually(t, "two songs landed", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.landed == 2 })
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.held) != 0 {
		t.Fatal("held unpaced", r.held)
	}
}

// The song playing is held only while it is well ahead of where it plays (paceLead of it): behind, or with its
// start or rate not known, never.
func TestTheSongPlayingIsFreeWhileBehind(t *testing.T) {
	r := &streamRun{s: &streamer{}}
	at := time.Now().Add(-60 * time.Second) // a minute into a song of 10 kB/s: 600 kB played
	for _, c := range []struct {
		playing bool
		at      time.Time
		bps     int64
		have    int64
		free    bool
	}{
		{true, at, 10000, 500000, true},     // behind where it plays
		{true, at, 10000, 850000, true},     // ahead, but by under paceLead (300 kB)
		{true, at, 10000, 1000000, false},   // ahead by 400 kB: held
		{false, at, 10000, 0, false},        // a song ahead of the one playing
		{true, time.Time{}, 10000, 0, true}, // not started: it is where the music will start
		{true, at, 0, 1 << 30, true},        // its rate not known
	} {
		r.prog = progress{playing: c.playing, playAt: c.at, playBps: c.bps, have: c.have}
		if got := r.paceFree(); got != c.free {
			t.Errorf("%+v: free %v", c, got)
		}
	}
}

// A round already running stops at its next cover once pacing begins: here the state turns paced at the gate's
// third look (the round's start, then one look before each cover), so one cover is asked for and no more.
func TestACoverRoundStopsWhenPacingBegins(t *testing.T) {
	r := newPaceRig(t, "catalogue", fakesonic.Library(30))
	r.f.Mu.Lock()
	r.f.Covers = map[string][]byte{}
	for i := range 10 {
		r.f.Covers[fmt.Sprintf("al%d", i)] = testJPEG(t, 160)
	}
	r.f.Mu.Unlock()
	batteryStatus = filepath.Join(r.tmp, "battery")
	t.Cleanup(func() { batteryStatus = "/sys/class/power_supply/battery/status" })
	os.WriteFile(batteryStatus, []byte("Charging\n"), 0o644)
	r.check(t) // the first copy and the catalogue
	looks := 0
	r.pace.Now = func() time.Time {
		if looks++; looks == 3 {
			r.say(t, paced)
		}
		return time.Unix(1795464061, 0)
	}
	r.check(t)
	if n := r.calls("getCoverArt"); n != 1 {
		t.Fatal("covers asked for in a round paced after its first:", n)
	}
}

// A clip for Caller Ten is background work too: while paced its 672 kB come at 200 KB/s (about two and a half
// seconds held past the first second's worth), and it is whole.
func TestAClipIsHeldWhilePaced(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	var held time.Duration
	var mu sync.Mutex
	now := time.Unix(1795464061, 0)
	r.pace.Every = 0
	r.pace.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r.pace.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		now, held = now.Add(d), held+d
		return ctx.Err()
	}
	if err := os.WriteFile(filepath.Join(r.tmp, "q2-bt-coex"), []byte(paced), 0o644); err != nil {
		t.Fatal(err)
	}
	r.ask(t, "#Q2CLIP/1\tseq=7\n0\ts1\t60000\t25000\n")
	r.bg.Wait()
	checkClip(t, filepath.Join(r.dir, "clips", "s1.mp3"), 60*24_000, 24_000)
	mu.Lock()
	defer mu.Unlock()
	if held < 2*time.Second || held > 3500*time.Millisecond {
		t.Fatal("held", held)
	}
}
