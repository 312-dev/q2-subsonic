package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/catalogue"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Radio: an endless list of songs like a seed, as the server picks them (getSimilarSongs2). Its
// seed is a song (Now Playing's Radio, and the car's, through the firmware's .radio request) or an
// artist (an artist page's Radio row). It starts with radioSongs songs, the seed song first; once
// radioLow or fewer are left after the one playing, the streamer asks for more (streamer.more),
// leaving out every song played or queued lately (radioRecent of them). While it plays, the state
// file says radio=<the seed's title>, which lights Now Playing's Radio pill (q2-coda's
// docs/internals.md#radio). Another list, a stop request or Sign Out ends it.

const (
	radioRecent = 200              // song ids a radio leaves out: the last played or queued
	radioIn     = 20 * time.Second // the server's time to answer a radio's ask
)

// radioState is the radio playing, if any, and the songs it leaves out.
type radioState struct {
	mu      sync.Mutex
	gen     int      // bumped by every start and end: a refill of an ended radio adds nothing
	on      bool     // a radio plays
	running bool     // its stream has started (and may since have ended)
	seed    string   // the id getSimilarSongs2 is asked about: a song's or an artist's
	title   string   // the state file's radio=
	last    string   // the song playing now, the second seed when the first brings too few new songs
	recent  []string // the songs played or queued lately, oldest first, at most radioRecent
	req     string   // the .radio request last read, or there at start-up
}

func (d *daemon) radioPath() string { return filepath.Join(d.tmp, "q2-"+appID+".radio") }

// radioInit takes a request left from before this start as answered, as slInit does .catplay: the
// firmware writes its request only once the app's socket is there.
func (d *daemon) radioInit() {
	if b, err := os.ReadFile(d.radioPath()); err == nil {
		d.radio.req = string(b)
	}
}

// heard notes id in the recent ring, the newest last; it is moved there when already in it.
func (r *radioState) heard(id string) {
	if id == "" {
		return
	}
	if i := slices.Index(r.recent, id); i >= 0 {
		r.recent = slices.Delete(r.recent, i, i+1)
	}
	r.recent = append(r.recent, id)
	if n := len(r.recent) - radioRecent; n > 0 {
		r.recent = slices.Delete(r.recent, 0, n)
	}
}

// played is the streamer's: a server song .playing named.
func (d *daemon) radioPlayed(id string) {
	d.radio.mu.Lock()
	defer d.radio.mu.Unlock()
	d.radio.heard(id)
	if d.radio.on {
		d.radio.last = id
	}
}

// fresh is songs less those played or queued lately and repeats within it, each noted as queued.
func (r *radioState) fresh(songs []subsonic.Song) []subsonic.Song {
	var out []subsonic.Song
	for _, s := range songs {
		if s.ID == "" || slices.Contains(r.recent, s.ID) {
			continue
		}
		r.heard(s.ID)
		out = append(out, s)
	}
	return out
}

// radioEnd ends the radio playing, if one is: the songs queued play on, and no more come.
func (d *daemon) radioEnd() {
	d.radio.mu.Lock()
	on := d.radio.on
	d.radio.on, d.radio.running, d.radio.title = false, false, ""
	d.radio.gen++
	d.radio.mu.Unlock()
	if on {
		d.state.setRadio("")
	}
}

// radioMore is a radio's refill, run by its stream: songs like the seed, then (when those are mostly
// played or queued lately) like the song playing now; nil once gen's radio has ended.
func (d *daemon) radioMore(gen int) []subsonic.Song {
	cl := d.client()
	d.radio.mu.Lock()
	seed, last, live := d.radio.seed, d.radio.last, d.radio.on && d.radio.gen == gen
	d.radio.mu.Unlock()
	if cl == nil || !live {
		return nil
	}
	var out []subsonic.Song
	for _, id := range []string{seed, last} {
		if id == "" || len(out) >= radioLow {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), radioIn)
		songs, err := cl.SimilarSongs(ctx, id, radioSongs)
		cancel()
		if err != nil && !errors.Is(err, subsonic.ErrNotFound) {
			d.log.Printf("radio: more like %s: %v", id, err)
			continue
		}
		d.radio.mu.Lock()
		if !d.radio.on || d.radio.gen != gen {
			d.radio.mu.Unlock()
			return nil
		}
		out = append(out, d.radio.fresh(songs)...)
		d.radio.mu.Unlock()
	}
	return out
}

// radioStart plays a radio of seed (a song's or an artist's id), titled title: first, when given, the
// seed song (playing from path when that is a card file), then the server's similar songs not played
// lately. The reply's message is what Now Playing toasts; an error when there is nothing to play.
func (d *daemon) radioStart(ctx context.Context, cl *subsonic.Client, seed, title string, first *subsonic.Song, path string) (string, error) {
	similar, err := cl.SimilarSongs(ctx, seed, radioSongs)
	if err != nil && !errors.Is(err, subsonic.ErrNotFound) { // Navidrome's answer when it knows none
		return "", err
	}
	d.radio.mu.Lock()
	var songs []subsonic.Song
	var paths []string
	if first != nil {
		d.radio.heard(first.ID)
		songs, paths = append(songs, *first), append(paths, path)
	}
	songs = append(songs, d.radio.fresh(similar)...)
	d.radio.mu.Unlock()
	if len(songs) <= 1 && first != nil || len(songs) == 0 {
		return "", errNoSimilar
	}
	d.radioEnd()
	seq := d.nextPlay()
	d.radio.mu.Lock()
	d.radio.gen++
	gen := d.radio.gen
	d.radio.on, d.radio.seed, d.radio.title, d.radio.last = true, seed, title, ""
	d.radio.mu.Unlock()
	d.state.setRadio(title)
	d.spawn(func() {
		d.playMu.Lock()
		defer d.playMu.Unlock()
		if d.latestPlay(seq) {
			d.stream.startRadio(songs, paths, func() []subsonic.Song { return d.radioMore(gen) })
		}
	})
	return "Playing " + songs[0].Title, nil
}

var errNoSimilar = errors.New("no similar songs")

// songOf is the server song a card path plays: a Library row's (the catalogue's path), a streamed or
// cached file's (the id in its name), or a synced song's; "" for a song that isn't the server's.
// keep is path when it can play as it is as the radio's first song: a card file outside the stream
// folder's top, which a new list empties (streamer.clean).
func (d *daemon) songOf(path string) (id, keep string) {
	if id := catalogue.SIDOf(d.card, appID, path); id != "" {
		return id, ""
	}
	if strings.HasPrefix(path, d.stream.dir()+"/") {
		if id := streamID(path); id != "" {
			if filepath.Dir(path) != d.stream.dir() && exists(path) {
				return id, path
			}
			return id, ""
		}
		return "", ""
	}
	for id, e := range d.manifest().Songs {
		if e.Path == path {
			return id, path
		}
	}
	return "", ""
}

// Answers to a .radio request, in the state file's radioack=<seq> <code>.
const (
	radioStarted = 0 // the radio plays
	radioNotOurs = 1 // the song is not a server song: the firmware plays a radio of its own library
	radioFailed  = 2 // no radio: the server has none, no similar songs, or it can't be reached
	// radioSignedOut: the song is the server's but nobody is signed in. A firmware that doesn't know
	// the code treats any code above 1 as radioFailed and toasts that no radio is available; it
	// does not fall back to its own library's radio, as it does on radioNotOurs.
	radioSignedOut = 3
)

// radioTick answers the firmware's .radio request, "<seq> start <path>" (a radio of the song at
// path, Now Playing's or the car's) or "<seq> stop", each once.
func (d *daemon) radioTick() {
	if !d.caps.Radio {
		return
	}
	d.radio.mu.Lock()
	ended := d.radio.on && d.radio.running && !d.stream.active() // its run ended: no more songs come
	d.radio.mu.Unlock()
	if ended {
		d.radioEnd()
	}
	b, err := os.ReadFile(d.radioPath())
	d.radio.mu.Lock()
	if err != nil || string(b) == d.radio.req {
		d.radio.mu.Unlock()
		return
	}
	d.radio.req = string(b)
	d.radio.mu.Unlock()
	line := strings.TrimRight(strings.SplitN(string(b), "\n", 2)[0], "\r")
	f := strings.SplitN(line, " ", 3)
	seq, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || len(f) < 2 {
		d.log.Printf("radio: bad request %q", line)
		return
	}
	ack := func(code int) { d.state.setRadioAck(fmt.Sprintf("%d %d", seq, code)) }
	switch {
	case f[1] == "stop":
		d.radioEnd()
		ack(radioStarted)
		return
	case f[1] != "start" || len(f) != 3:
		d.log.Printf("radio: bad request %q", line)
		return
	}
	id, keep := d.songOf(f[2])
	cl := d.client()
	switch {
	case id == "":
		ack(radioNotOurs)
		return
	case cl == nil:
		ack(radioSignedOut)
		return
	}
	d.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), radioIn)
		defer cancel()
		sg, err := cl.Song(ctx, id)
		if err == nil {
			_, err = d.radioStart(ctx, cl, id, sg.Title, &sg, keep)
		}
		if err != nil {
			d.log.Printf("radio: like %s: %v", id, err)
			ack(radioFailed)
			return
		}
		ack(radioStarted)
	})
}

// artistRadio is an artist page's Radio row: a radio of songs like the artist's.
func (d *daemon) artistRadio(ctx context.Context, cl *subsonic.Client, arg string) q2spot.Reply {
	_, id := parseKey(arg)
	name := "Artist"
	if it, err := d.item(ctx, cl, "artist", id); err == nil && it.name != "" {
		name = it.name
	}
	msg, err := d.radioStart(ctx, cl, id, name, nil, "")
	switch {
	case subsonic.Unsupported(err):
		return q2spot.Fail(q2spot.Unavailable, notHere)
	case errors.Is(err, errNoSimilar):
		return q2spot.Fail(q2spot.Unavailable, "No similar songs found")
	case err != nil:
		return d.errToast(err)
	}
	return q2spot.Reply{Message: msg}
}
