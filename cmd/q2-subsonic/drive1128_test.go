package main

import (
	"os"
	"testing"
	"time"
)

// Download Ahead under the Q2's Repeat All (the drive of 2026-11-22: a radio that stopped at its fourth
// song). The firmware's queue holds the songs downloaded so far, and once its play order has nothing
// unplayed left, its .ahead names Repeat All's next pass: songs already whole on the card. Those must
// not take Download Ahead's slots while the list still has songs to fetch.

// fourWhole starts a list of six songs, fetches s0 to s3 as the firmware's .ahead asks, and then plays
// s3, the last of them queued. k(i) is song i's path.
func fourWhole(t *testing.T, cache bool) (e *env, seq string, k func(int) string) {
	t.Helper()
	e = aheadEnv(t, false)
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
	os.WriteFile(e.playing, []byte(k(3)+"\n"), 0o644)
	e.ahead(t, seq, 1, k(0), k(1))
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
