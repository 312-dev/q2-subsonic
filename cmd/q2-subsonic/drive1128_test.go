package main

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// Download Ahead under the Q2's Repeat All (the drive of 2026-11-22: a radio that stopped at its fourth
// song). The firmware's queue holds the songs downloaded so far, and once its play order has nothing
// unplayed left, its .ahead names Repeat All's next pass: songs already whole on the card. Those must
// not take Download Ahead's slots while the list still has songs to fetch.

// fourWhole starts a list of six songs, fetches s0 to s3 as the firmware's .ahead asks, and then plays
// s3, the last of them queued. k(i) is song i's path. set, if any, changes the streamer before it starts.
func fourWhole(t *testing.T, cache bool, set ...func(*streamer)) (e *env, seq string, k func(int) string) {
	t.Helper()
	e = aheadEnv(t, false)
	for _, f := range set {
		f(e.s)
	}
	k = func(i int) string { return e.stream(i, "flac") }
	if cache {
		e.cacheOn(1<<30, 1<<40)
		k = func(i int) string { return e.kept(i, "flac", "original") }
	}
	e.s.start(songs(6, "flac"), 0)
	seq = e.playSeq(t)
	os.WriteFile(e.playing, []byte(k(0)+"\n"), 0o644)
	e.ahead(t, seq, 3, k(1), k(2), k(3))
	until(t, "s0 to s3 fetched", func() bool { return exists(k(0)) && exists(k(1)) && exists(k(2)) && exists(k(3)) })
	return e, seq, k
}

// waitTries waits up to d for id's first fetch; its count of fetches then.
func waitTries(e *env, id string, d time.Duration) int {
	for end := time.Now().Add(d); time.Now().Before(end) && e.src.tries(id) == 0; {
		time.Sleep(20 * time.Millisecond)
	}
	return e.src.tries(id)
}

// Repeat All's next pass, s0 and s1, both whole: s4 and s5, the list's next, are fetched.
func TestAheadRepeatAllsNextPassTakesNoSlot(t *testing.T) {
	e, seq, k := fourWhole(t, true)
	os.WriteFile(e.playing, []byte(k(3)+"\n"), 0o644)
	e.ahead(t, seq, 2, k(0), k(1))
	if waitTries(e, "s4", 2*time.Second) == 0 {
		t.Fatalf("s4 never fetched; GETs: %v", e.src.got())
	}
	until(t, "s5 fetched", func() bool { return e.src.tries("s5") > 0 })
}

// Repeat off: nothing after s3 in the play order, an empty .ahead, and s4 and s5 fetched, as before.
func TestAheadEmptyAtTheQueuesEndFetchesTheListsNext(t *testing.T) {
	e, seq, k := fourWhole(t, true)
	os.WriteFile(e.playing, []byte(k(3)+"\n"), 0o644)
	e.ahead(t, seq, 2)
	until(t, "s4 and s5 fetched", func() bool { return e.src.tries("s4") > 0 && e.src.tries("s5") > 0 })
}

// n bounds the songs to fetch: n=1 fetches s4 and not s5, whole songs named or not.
func TestAheadWholeSongsNamedStillBoundedByN(t *testing.T) {
	e, seq, k := fourWhole(t, true)
	// .ahead before .playing, as the firmware writes them (addon.c stream_poll: ahead_poll first): the
	// other way round, the app can see s3 playing under the old .ahead's n=3 and fetch s5
	e.ahead(t, seq, 1, k(0), k(1))
	os.WriteFile(e.playing, []byte(k(3)+"\n"), 0o644)
	if waitTries(e, "s4", 2*time.Second) == 0 {
		t.Fatalf("s4 never fetched; GETs: %v", e.src.got())
	}
	time.Sleep(200 * time.Millisecond)
	if n := e.src.tries("s5"); n != 0 {
		t.Errorf("s5 fetched %d times; n=1 is s4 alone", n)
	}
}

// Every song of the list whole: nothing more is fetched.
func TestAheadAllWholeFetchesNothing(t *testing.T) {
	e := aheadEnv(t, false)
	e.cacheOn(1<<30, 1<<40)
	k := func(i int) string { return e.kept(i, "flac", "original") }
	e.s.start(songs(3, "flac"), 0)
	seq := e.playSeq(t)
	os.WriteFile(e.playing, []byte(k(0)+"\n"), 0o644)
	e.ahead(t, seq, 2, k(1), k(2))
	until(t, "s0 to s2 fetched", func() bool { return exists(k(0)) && exists(k(1)) && exists(k(2)) })
	os.WriteFile(e.playing, []byte(k(2)+"\n"), 0o644)
	e.ahead(t, seq, 2, k(0), k(1))
	time.Sleep(300 * time.Millisecond)
	for _, id := range []string{"s0", "s1", "s2"} {
		if n := e.src.tries(id); n != 1 {
			t.Errorf("%s fetched %d times; once is all a whole list needs", id, n)
		}
	}
}

// Stream Cache off: the songs behind the one playing are deleted, but not those .ahead names to come
// (Repeat All's next pass), which were fetched over and over in a loop.
func TestAheadCacheOffKeepsTheSongsNamedToCome(t *testing.T) {
	e, seq, k := fourWhole(t, false)
	e.ahead(t, seq, 2, k(0), k(1)) // before .playing, as stream_poll writes them (ahead_poll first)
	os.WriteFile(e.playing, []byte(k(3)+"\n"), 0o644)
	time.Sleep(500 * time.Millisecond)
	if n := e.src.tries("s0"); n != 1 {
		t.Errorf("s0 fetched %d times; it is whole and named to come", n)
	}
	if !exists(k(0)) || !exists(k(1)) {
		t.Errorf("s0 or s1 deleted while .ahead names them to come")
	}
	if e.src.tries("s4") == 0 {
		t.Errorf("s4 never fetched; GETs: %v", e.src.got())
	}
}

// A song whole on the card while its fetch has yet to return keeps its slot: the download's watcher
// plans in that moment too, and saw s4 whole and not yet landed, as a song of Repeat All's next pass
// that takes none, gave its slot to s5, cut s4's fetch as left, and fetched s5 (n=1).
func TestAheadSongWholeBeforeItsFetchReturnsKeepsItsSlot(t *testing.T) {
	var late atomic.Bool // s0 to s3 land at once; s4 waits after it is whole
	e, seq, k := fourWhole(t, true, func(s *streamer) {
		s.statusEvery = 5 * time.Millisecond // the download's watcher plans every 5 ms
		s.landed = func() {
			if late.Load() {
				time.Sleep(100 * time.Millisecond)
			}
		}
	})
	late.Store(true)
	e.ahead(t, seq, 1, k(0), k(1))
	os.WriteFile(e.playing, []byte(k(3)+"\n"), 0o644)
	if waitTries(e, "s4", 2*time.Second) == 0 {
		t.Fatalf("s4 never fetched; GETs: %v", e.src.got())
	}
	time.Sleep(400 * time.Millisecond)
	if n := e.src.tries("s5"); n != 0 {
		t.Errorf("s5 fetched %d times; n=1 is s4 alone", n)
	}
}
