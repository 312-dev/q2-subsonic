package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// stateKey is the state file's value for key, "" when it has none.
func (d *tdaemon) stateKey(key string) string {
	b, _ := os.ReadFile(filepath.Join(d.tmp, "q2-subsonic.state"))
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// ask writes the firmware's .radio request and has the app read it, as its tick does.
func (d *tdaemon) ask(line string) {
	os.WriteFile(filepath.Join(d.tmp, "q2-subsonic.radio"), []byte(line+"\n"), 0o644)
	d.radioTick()
}

// queueLines is the running stream's queue file, a card path a line.
func (d *tdaemon) queueLines() []string {
	m, _ := filepath.Glob(filepath.Join(d.card, ".subsonic", "stream", "queue-*.m3u"))
	if len(m) != 1 {
		return nil
	}
	b, _ := os.ReadFile(m[0])
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// Now Playing's Radio on a streamed song: getSimilarSongs2 is asked about the song's id, the queue is
// the song then the similar ones, the state file says radio= and answers the request.
func TestSongRadioFromNowPlaying(t *testing.T) {
	n := &nav{albums: 4, songsPer: 6, simStep: true}
	d := newTestDaemon(t, n, "playlist radio", true)
	d.ask("7 start " + d.streamed("al3.2"))
	eventually(t, "the request answered", func() bool { return d.stateKey("radioack") == "7 0" })
	if q := n.query("getSimilarSongs2"); q.Get("id") != "al3.2" || q.Get("count") != "50" {
		t.Fatalf("query %v", q)
	}
	if got := d.stateKey("radio"); got != "Song 2" {
		t.Fatalf("radio=%q", got)
	}
	d.waitQueued(t, 13) // the seed, then albums 0 and 1
	if q := d.queueLines(); streamID(q[0]) != "al3.2" || streamID(q[1]) != "al0.0" {
		t.Fatalf("queue %q", q)
	}
}

// A radio asks for more once radioLow songs or fewer are left after the one playing, and never
// queues a song it played or queued lately: the fake's answers overlap by half.
func TestRadioRefillsWithoutRepeats(t *testing.T) {
	n := &nav{albums: 5, songsPer: 6, simStep: true}
	d := newTestDaemon(t, n, "playlist radio", true)
	d.stream.gap = 0
	d.ask("7 start " + d.streamed("al3.2"))
	d.waitQueued(t, 13)
	if n.reqs("getSimilarSongs2") != 1 {
		t.Fatalf("asked %d times before any song played", n.reqs("getSimilarSongs2"))
	}
	q := d.queueLines()
	eventually(t, "the third song fetched", func() bool { return exists(q[2]) })
	os.WriteFile(d.stream.playingFile, []byte(q[2]+"\n"), 0o644) // 10 left after it
	// The 2nd answer, albums 1 and 2: album 1 again, album 2 new (6); too few, so the song playing
	// seeds a 3rd, albums 2 and 3: album 3 new but for the seed (5).
	d.waitQueued(t, 24)
	var ids []string
	for _, l := range d.queueLines() {
		ids = append(ids, streamID(l))
	}
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	if len(slices.Compact(sorted)) != len(ids) {
		t.Fatalf("a song queued twice: %q", ids)
	}
	if q := n.query("getSimilarSongs2"); q.Get("id") != "al0.1" {
		t.Fatalf("the second seed is %q, not the song playing", q.Get("id"))
	}
	if d.stateKey("radio") != "Song 2" {
		t.Fatal("radio ended")
	}
}

// Stop ends the radio (its songs play on); another list ends it too; a song that isn't the server's
// is answered 1, for the firmware's own radio.
func TestRadioStopAndNotOurs(t *testing.T) {
	n := &nav{albums: 4, songsPer: 6, simStep: true}
	d := newTestDaemon(t, n, "playlist radio", true)
	d.ask("7 start " + d.streamed("al3.2"))
	d.waitQueued(t, 13)
	d.ask("8 stop")
	if d.stateKey("radioack") != "8 0" || d.stateKey("radio") != "" {
		t.Fatalf("after stop: radioack=%q radio=%q", d.stateKey("radioack"), d.stateKey("radio"))
	}
	d.ask("9 start /mnt/mmc/Music/Someone/Song.flac")
	if d.stateKey("radioack") != "9 1" {
		t.Fatalf("a card song: radioack=%q", d.stateKey("radioack"))
	}
	d.ask("10 start " + d.streamed("al3.2"))
	eventually(t, "the radio on", func() bool { return d.stateKey("radio") == "Song 2" })
	d.do("shuffle", "")
	eventually(t, "another list ends the radio", func() bool { return d.stateKey("radio") == "" })
	asked := n.reqs("getSimilarSongs2")
	d.ask("10 start " + d.streamed("al3.2")) // the same request again: answered once
	if n.reqs("getSimilarSongs2") != asked {
		t.Fatalf("asked again: %d times", n.reqs("getSimilarSongs2"))
	}
}

// Without the firmware's "radio" word the app reads no request.
func TestRadioNeedsTheWord(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "playlist", true)
	d.ask("7 start " + d.streamed("al0.1"))
	if d.stateKey("radioack") != "" {
		t.Fatal("answered without the word")
	}
}

// seedPath is where the stream put song id: its stream cache file (one subfolder under the account's,
// hashed), or the stream folder's top when the cache is Off. "" until it is there.
func (d *tdaemon) seedPath(id string) string {
	for _, pat := range []string{
		filepath.Join(d.card, ".subsonic", "stream", "*", "*", "*", "* [[]"+id+"].flac"),
		filepath.Join(d.card, ".subsonic", "stream", "* [[]"+id+"].flac"),
	} {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// Radio on the song a list is playing, which the stream already has on the card, takes that file as
// the radio's first song: the server is asked for it once, by the list's own stream, whether the
// stream kept it in its cache or (Stream Cache Off) in the stream folder's top.
func TestRadioSeedIsNotFetchedAgain(t *testing.T) {
	for name, cacheMB := range map[string]int{"cache on": 0, "cache off": -1} {
		t.Run(name, func(t *testing.T) {
			n := &nav{albums: 4, songsPer: 6, simStep: true}
			d := newTestDaemon(t, n, "playlist radio", true)
			if err := d.saveSettings(settings{StreamCacheMB: cacheMB}); err != nil {
				t.Fatal(err)
			}
			d.do("play", "album:al3 2")
			eventually(t, "the seed streamed", func() bool { return d.seedPath("al3.2") != "" })
			path := d.seedPath("al3.2")
			os.WriteFile(d.stream.playingFile, []byte(path+"\n"), 0o644) // the firmware plays it
			d.ask("7 start " + path)
			eventually(t, "the request answered", func() bool { return d.stateKey("radioack") == "7 0" })
			d.waitQueued(t, 13)
			if q := d.queueLines(); streamID(q[0]) != "al3.2" || !exists(q[0]) {
				t.Fatalf("the radio's first song %q is not the seed on the card", q[0])
			}
			if got := n.fetches("al3.2"); got != 1 {
				t.Fatalf("the seed was fetched %d times, want 1", got)
			}
		})
	}
}

// Radio on a song the list's own stream is still writing into place (a Q2 that plays a song as it
// arrives): the radio waits for that download rather than start a second one, and its first song is
// the one file, whole.
func TestRadioSeedStillArrivingIsNotFetchedAgain(t *testing.T) {
	n := &nav{albums: 4, songsPer: 6, simStep: true, half: make(chan struct{})}
	d := newTestDaemon(t, n, "playlist radio ahead grow", true)
	d.do("play", "album:al3 2")
	eventually(t, "the seed arriving", func() bool {
		p := d.seedPath("al3.2")
		return p != "" && !d.stream.whole(p)
	})
	path := d.seedPath("al3.2")
	os.WriteFile(d.stream.playingFile, []byte(path+"\n"), 0o644)
	d.ask("7 start " + path)
	eventually(t, "the request answered", func() bool { return d.stateKey("radioack") == "7 0" })
	time.Sleep(300 * time.Millisecond) // a second download would have started by now
	if got := n.fetches("al3.2"); got != 1 {
		t.Fatalf("the seed was fetched %d times while it arrived, want 1", got)
	}
	close(n.half)
	d.waitQueued(t, 13)
	q := d.queueLines()
	if q[0] != path {
		t.Fatalf("the radio's first song is %q, not the file arriving at %q", q[0], path)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 1000 {
		t.Fatalf("the seed's file: %v, %v", fi, err)
	}
	if got := n.fetches("al3.2"); got != 1 {
		t.Fatalf("the seed was fetched %d times, want 1", got)
	}
}

// The drive of 2026-11-22: a radio started on a song, stopped from the car, then started on the song again, twice,
// and each time the firmware toasted "Radio isn't available for this song" (radioack code 2). A server answers the
// same similar songs for the same seed, and the first radio had queued them all, so every one was "played or queued
// lately" and the new radio held the seed alone (errNoSimilar). A radio started again plays those songs again.
func TestRadioStartedAgainOnTheSameSong(t *testing.T) {
	n := &nav{albums: 4, songsPer: 6} // the same answer every time, as a real server's for one seed
	d := newTestDaemon(t, n, "playlist radio", true)
	d.ask("7 start " + d.streamed("al3.2"))
	eventually(t, "the first radio on", func() bool { return d.stateKey("radioack") == "7 0" })
	d.ask("8 stop")
	for seq := 9; seq <= 10; seq++ {
		d.ask(strconv.Itoa(seq) + " start " + d.streamed("al3.2"))
		want := strconv.Itoa(seq) + " 0"
		eventually(t, "radio "+want, func() bool { return strings.HasPrefix(d.stateKey("radioack"), strconv.Itoa(seq)+" ") })
		if got := d.stateKey("radioack"); got != want {
			t.Fatalf("started again: radioack=%q, want %q", got, want)
		}
		if d.stateKey("radio") != "Song 2" {
			t.Fatalf("radio=%q", d.stateKey("radio"))
		}
		d.ask(strconv.Itoa(seq) + "0 stop")
	}
}

// A Radio whose server is slow to answer does not replace a list the user started meanwhile: its
// place in line is taken at the press, and its answer, older than the list, is dropped.
func TestASlowRadioDoesNotReplaceALaterList(t *testing.T) {
	n := &nav{albums: 4, songsPer: 6, simHold: make(chan struct{})}
	d := newTestDaemon(t, n, "playlist radio", true)
	d.ask("7 start " + d.streamed("al3.2"))
	eventually(t, "the radio asked the server", func() bool { return n.reqs("getSimilarSongs2") == 1 })
	album := []subsonic.Song{{ID: "al1.0", Title: "Song 0", Suffix: "flac"}, {ID: "al1.1", Title: "Song 1", Suffix: "flac"}}
	d.startStream(d.nextPlay(), album, 0) // the user starts a list while the radio waits
	d.waitQueued(t, 2)
	close(n.simHold)
	eventually(t, "the radio answered", func() bool { return d.stateKey("radioack") != "" })
	time.Sleep(200 * time.Millisecond)
	if q := d.queueLines(); len(q) != 2 || streamID(q[0]) != "al1.0" {
		t.Fatalf("the list the user started was replaced: %v", q)
	}
}

// A radio's start holds the stream's lock and takes the radio's (streamer.started), so the tick that
// reads the radio's state must not hold the radio's lock while it takes the stream's, or each waits
// for the other for good. Many starts, with ticks running beside them, are all answered.
func TestARadioStartAndTheTickNeverWaitForEachOther(t *testing.T) {
	n := &nav{albums: 4, songsPer: 6}
	d := newTestDaemon(t, n, "playlist radio", true)
	done, stop := make(chan struct{}), make(chan struct{})
	for range 4 { // ticks, as the daemon's and the requests' come
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
					d.radioTick()
				}
			}
		}()
	}
	go func() {
		defer close(done)
		defer close(stop)
		for seq := 1; seq <= 300; seq++ {
			d.ask(strconv.Itoa(seq) + " start " + d.streamed("al3.2"))
			for end := time.Now().Add(20 * time.Second); !strings.HasPrefix(d.stateKey("radioack"), strconv.Itoa(seq)+" "); {
				if time.Now().After(end) {
					return // the select below says so
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the radio's start and the tick each wait for the other's lock")
	}
}
