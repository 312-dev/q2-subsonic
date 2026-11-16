package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
