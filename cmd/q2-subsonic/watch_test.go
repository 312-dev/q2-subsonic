package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// A song without lyrics adds a line to lyrics-none.txt rather than writing the whole file again;
// the file is written whole (dropping the oldest) only past a quarter more than lyricsNoneMax.
func TestLyricsNoneAppends(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	var b strings.Builder
	now := time.Now().Unix()
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "acct old%d %d\n", i, now)
	}
	old := b.String()
	os.WriteFile(d.lyricsNonePath(), []byte(old), 0o644)
	for i := 0; i < 100; i++ {
		d.lyricsNoneAdd(fmt.Sprintf("acct new%d", i))
	}
	got, _ := os.ReadFile(d.lyricsNonePath())
	if !strings.HasPrefix(string(got), old) || strings.Count(string(got), "\n") != 4100 {
		t.Fatalf("written again whole: %d lines", strings.Count(string(got), "\n"))
	}
	d.lyr.none, d.lyr.lines = nil, 0 // read again, as a restart reads it
	if !d.lyricsNoneHas("acct new99") || !d.lyricsNoneHas("acct old0") {
		t.Fatal("a key lost")
	}
	for i := 100; i < 2300; i++ { // past 6,250 lines: written whole, the 5,000 newest
		d.lyricsNoneAdd(fmt.Sprintf("acct new%d", i))
	}
	got, _ = os.ReadFile(d.lyricsNonePath())
	if n := strings.Count(string(got), "\n"); n > lyricsNoneMax+lyricsNoneMax/4 {
		t.Fatalf("%d lines", n)
	}
}

// A check the server did not answer is retried soon, not at the quiet schedule's 10 minutes: the owner's
// 2026-11-24, a ping that timed out just after the Q2 joined a new network left the library down for 4 min 36 s
// while the server answered, until Sync Now. The server back, the retry connects.
func TestAFailedCheckIsRetriedSoon(t *testing.T) {
	r := newCatRig(t, "playlist catalogue", nil)
	r.check(t)
	r.mu.Lock()
	r.lastReq = time.Now().Add(-slUnused - time.Second) // quiet: the schedule is slUnusedEvery
	r.mu.Unlock()
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.check(t)
	r.sl.mu.Lock()
	next, up := time.Until(r.sl.nextPing), r.sl.up
	r.sl.mu.Unlock()
	if up != -1 {
		t.Fatalf("up %d after a check of a server down", up)
	}
	if next > slRetryFirst {
		t.Fatalf("the next check in %v after a failed one", next)
	}
	r.f.Mu.Lock()
	r.f.Down = false
	r.f.Mu.Unlock()
	r.sl.mu.Lock()
	r.sl.nextPing = time.Now().Add(-time.Second) // slRetryFirst on
	r.sl.mu.Unlock()
	r.slTick()
	r.bg.Wait()
	r.sl.mu.Lock()
	up = r.sl.up
	r.sl.mu.Unlock()
	if up != 1 {
		t.Fatalf("the retry did not connect: up %d", up)
	}
	b, _ := os.ReadFile(filepath.Join(r.dir, "log.txt"))
	for _, want := range []string{"server library: no answer (1 in a row, for 0s): the next check in 15s",
		"server library: the server answers again after 1 failed checks"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("log.txt has no %q:\n%s", want, b)
		}
	}
}

// The retries double from slRetryFirst to slRetryMost, never past the schedule's own wait, and after
// slRetryFor of failures the schedule is back: a server out of reach for hours costs a few pings more than
// it, not one every two minutes.
func TestRetriesSettleToTheSchedule(t *testing.T) {
	for _, c := range []struct {
		fails       int
		down, every time.Duration
		want        time.Duration
	}{
		{1, 0, slUnusedEvery, 15 * time.Second},
		{2, 15 * time.Second, slUnusedEvery, 30 * time.Second},
		{3, 45 * time.Second, slUnusedEvery, time.Minute},
		{4, 105 * time.Second, slUnusedEvery, 2 * time.Minute},
		{9, 9 * time.Minute, slUnusedEvery, 2 * time.Minute},
		{30, 0, slUnusedEvery, 2 * time.Minute}, // no overflow however many
		{10, slRetryFor, slUnusedEvery, slUnusedEvery},
		{12, 3 * time.Hour, slUnusedEvery, slUnusedEvery},
		{3, 45 * time.Second, slEvery, slEvery}, // in use: never later than the minute's ping
		{0, 0, slEvery, slEvery},
	} {
		if got := slRetryIn(c.fails, c.down, c.every); got != c.want {
			t.Errorf("slRetryIn(%d, %v, %v) = %v, want %v", c.fails, c.down, c.every, got, c.want)
		}
	}
	// and the daemon keeps to it: a long outage's next check is the quiet schedule's
	r := newCatRig(t, "playlist catalogue", nil)
	r.check(t)
	r.mu.Lock()
	r.lastReq = time.Now().Add(-slUnused - time.Second)
	r.mu.Unlock()
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.check(t)
	r.sl.mu.Lock()
	r.sl.downAt = time.Now().Add(-slRetryFor - time.Minute) // down for 11 minutes
	r.sl.mu.Unlock()
	r.check(t)
	r.sl.mu.Lock()
	next := time.Until(r.sl.nextPing)
	r.sl.mu.Unlock()
	if next < slUnusedEvery-time.Minute {
		t.Fatalf("a long outage's next check in %v", next)
	}
}

// A Wi-Fi lease (udhcpc rewrites /tmp/resolv.conf) while the server is down asks it at once, the retries
// started over: no wait for the backoff when a network is joined.
func TestANewLeaseChecksAtOnce(t *testing.T) {
	r := newCatRig(t, "playlist catalogue", nil)
	resolv := filepath.Join(r.tmp, "resolv.conf")
	os.WriteFile(resolv, []byte("nameserver 10.0.0.1\n"), 0o644)
	r.check(t)
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.check(t)
	r.check(t) // two failures: the next in 30 s
	r.f.Mu.Lock()
	r.f.Down = false
	r.f.Mu.Unlock()
	r.slTick() // nothing new: not yet
	r.bg.Wait()
	r.sl.mu.Lock()
	up := r.sl.up
	r.sl.mu.Unlock()
	if up != -1 {
		t.Fatalf("checked before its time: up %d", up)
	}
	later := time.Now().Add(2 * time.Second)
	os.WriteFile(resolv, []byte("nameserver 10.0.0.2\n"), 0o644)
	os.Chtimes(resolv, later, later)
	r.slTick()
	r.bg.Wait()
	r.sl.mu.Lock()
	up, fails := r.sl.up, r.sl.fails
	r.sl.mu.Unlock()
	if up != 1 || fails != 0 {
		t.Fatalf("a new lease: up %d, %d failures", up, fails)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "log.txt")); !strings.Contains(string(b), "a new Wi-Fi lease with the server down") {
		t.Fatalf("log.txt:\n%s", b)
	}
	if !slices.Contains(func() []string { _, n := r.pressNames(); return n }(), "resolv.conf") {
		t.Fatal("resolv.conf is not watched: a lease waits for the next tick")
	}
}
