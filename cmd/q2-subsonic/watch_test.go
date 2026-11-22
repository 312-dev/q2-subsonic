package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/fw"
)

// A press (.catplay renamed into place, as the firmware writes it) kicks the tick at once; another
// file in the folder doesn't.
func TestAPressKicksTheTickAtOnce(t *testing.T) {
	dir := t.TempDir()
	kick := make(chan struct{}, 1)
	go watchNames(dir, []string{"q2-subsonic.catplay"}, kick, nil)
	time.Sleep(50 * time.Millisecond) // the watch in place
	os.WriteFile(filepath.Join(dir, "other.tmp"), []byte("x"), 0o644)
	os.Rename(filepath.Join(dir, "other.tmp"), filepath.Join(dir, "q2-other.state"))
	select {
	case <-kick:
		t.Fatal("kicked by another file")
	case <-time.After(100 * time.Millisecond):
	}
	os.WriteFile(filepath.Join(dir, "c.tmp"), []byte("7 0\n/x\n"), 0o644)
	at := time.Now()
	os.Rename(filepath.Join(dir, "c.tmp"), filepath.Join(dir, "q2-subsonic.catplay"))
	select {
	case <-kick:
		if d := time.Since(at); d > 100*time.Millisecond {
			t.Fatalf("kicked %v after the press", d)
		}
	case <-time.After(time.Second):
		t.Fatal("not kicked by the press")
	}
}

// The build is in the version the app says: Settings' row and the log's start line.
func TestTheVersionSaysTheBuild(t *testing.T) {
	saved := build
	defer func() { build = saved }()
	build = ""
	if versionBuild() != version {
		t.Fatalf("no build: %q", versionBuild())
	}
	build = "7ded418-dirty"
	if versionBuild() != version+"+7ded418-dirty" {
		t.Fatalf("%q", versionBuild())
	}
}

// The 60 s ping (a request and a line in log.txt on the card) goes every 10 minutes once nothing has
// used the app for 5 and nothing streams, and at once when it is used again.
func TestThePingBacksOffWhileTheQ2IsUnused(t *testing.T) {
	r := newCatRig(t, "playlist catalogue", nil)
	r.check(t)
	r.sl.mu.Lock()
	next := time.Until(r.sl.nextPing)
	r.sl.mu.Unlock()
	if next > slEvery {
		t.Fatalf("in use: the next ping in %v", next)
	}
	r.mu.Lock()
	r.lastReq = time.Now().Add(-slUnused - time.Second)
	r.mu.Unlock()
	r.check(t)
	r.sl.mu.Lock()
	next = time.Until(r.sl.nextPing)
	r.sl.mu.Unlock()
	if next < slUnusedEvery-time.Minute {
		t.Fatalf("unused: the next ping in %v", next)
	}
	r.mu.Lock()
	r.lastReq = time.Now() // a request
	r.mu.Unlock()
	r.slTick()
	r.bg.Wait()
	r.sl.mu.Lock()
	next = time.Until(r.sl.nextPing)
	r.sl.mu.Unlock()
	if next > slEvery {
		t.Fatalf("used again: the next ping in %v", next)
	}
}

// With the daemon's watch on, a list with nothing to fetch looks again idleLooks polls on, and a
// change of .playing wakes it at once.
func TestAnIdleRunLoopWakesOnPlaying(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.poll = 200 * time.Millisecond // an idle look then 1 s
	dir := filepath.Dir(e.playing)
	go watchNames(dir, e.s.watched(), e.s.kick, &e.s.kicked)
	until(t, "the watch", e.s.kicked.Load)
	e.s.start(songs(3, "flac"), 0)
	until(t, "all fetched", func() bool { return exists(e.stream(2, "flac")) })
	var reported []string
	var mu sync.Mutex
	e.s.played = func(id string) { mu.Lock(); reported = append(reported, id); mu.Unlock() }
	time.Sleep(300 * time.Millisecond) // the loop asleep with nothing to fetch
	at := time.Now()
	os.WriteFile(e.playing, []byte(e.stream(1, "flac")+"\n"), 0o644)
	until(t, "s1 seen", func() bool { mu.Lock(); defer mu.Unlock(); return len(reported) > 0 })
	if d := time.Since(at); d > 150*time.Millisecond {
		t.Fatalf(".playing seen %v after it changed", d)
	}
}
