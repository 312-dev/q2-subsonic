package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Resume: after the Q2 restarts, the firmware's Memory Playback restores the queue that played, but
// only the songs whose files are on the card; a stream's other songs, not downloaded yet, it asks of
// the app (q2-coda's docs/internals.md#memory-playback-and-a-streamed-queue), whose q2app says
// resume=1. The request is /tmp/q2-subsonic.resume, "<seq> <index> <seconds> paused|playing", then
// every saved song's card path a line in the queue's order: a streamed or cached file (the server id
// in its name), a Library server row, a synced song or any other card file. The app plays the list on
// from index as a catalogue queue answering seq: its queue file's first song is the one the firmware
// restored, which it keeps as it is, paused at its place, and the songs after it download ahead as for
// any list. Nothing here plays: .playing names no song until the firmware plays one, so no play is
// reported for the song waiting paused. Answered in the state file's resumeack=<seq> <code>.

const (
	resumeOK      = 0 // the queue file follows
	resumeNotOurs = 1 // no song of the list is the server's
	resumeFailed  = 2 // signed out, the song restored unknown to the server, or the server not answering
)

// resumeIn is how long finding the list's songs may take: the catalogue's own record first, then the
// server for each song it doesn't have.
// ponytail: a song the catalogue lacks is asked of the server one at a time; a long list played from
// outside the Library (an artist page, a radio) can run out of this and be declined.
const resumeIn = 30 * time.Second

type resumeState struct {
	mu  sync.Mutex
	req string // the .resume request last read, or there at start-up
}

func (d *daemon) resumePath() string { return filepath.Join(d.tmp, "q2-"+appID+".resume") }

// resumeInit takes a request left from before this start as answered, as radioInit does: the
// firmware writes its request only once the app's socket is there.
func (d *daemon) resumeInit() {
	if b, err := os.ReadFile(d.resumePath()); err == nil {
		d.resume.req = string(b)
	}
}

// resumeTick answers the firmware's .resume request, each once.
func (d *daemon) resumeTick() {
	if !d.caps.Playlist {
		return
	}
	b, err := os.ReadFile(d.resumePath())
	d.resume.mu.Lock()
	if err != nil || string(b) == d.resume.req {
		d.resume.mu.Unlock()
		return
	}
	d.resume.req = string(b)
	d.resume.mu.Unlock()
	ls := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	f := strings.Fields(ls[0])
	if len(f) != 4 || (f[3] != "paused" && f[3] != "playing") {
		d.log.Printf("resume: bad request %q", ls[0])
		return
	}
	seq, err1 := strconv.ParseInt(f[0], 10, 64)
	at, err2 := strconv.Atoi(f[1])
	paths := ls[1:]
	if err1 != nil || err2 != nil || at < 0 || at >= len(paths) {
		d.log.Printf("resume: bad request %q", ls[0])
		return
	}
	ack := func(code int) { d.state.setResumeAck(fmt.Sprintf("%d %d", seq, code)) }
	ids, keep := make([]string, len(paths)), make([]string, len(paths))
	ours := false
	for i, p := range paths {
		ids[i], keep[i] = d.songOf(p)
		ours = ours || ids[i] != ""
	}
	cl := d.client()
	switch {
	case !ours:
		ack(resumeNotOurs)
		return
	case cl == nil:
		ack(resumeFailed)
		return
	}
	d.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), resumeIn)
		defer cancel()
		songs, local, start, err := d.resumeSongs(ctx, cl, paths, ids, keep, at)
		if err != nil {
			d.log.Printf("resume: %v", err)
			ack(resumeFailed)
			return
		}
		n := d.nextPlay()
		d.playMu.Lock()
		if d.latestPlay(n) {
			d.stream.startCat(songs, local, start, &catPlay{seq: seq, local: d.stream.copies()})
		}
		d.playMu.Unlock()
		d.log.Printf("resume: %d songs from %d, %s", len(songs), start, f[3])
		ack(resumeOK)
	})
}

// resumeSongs is the request's list as the stream plays it: each server song (ids[i]) from the
// catalogue's record, else the server's, played from keep[i] when that file is on the card already;
// each other card file as it is. A song the server no longer has is left out, but the one restored
// (at), the queue's first, must be there. start is that song's place in the list.
func (d *daemon) resumeSongs(ctx context.Context, cl *subsonic.Client, paths, ids, keep []string, at int) (songs []subsonic.Song, local []string, start int, err error) {
	want := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			want[id] = true
		}
	}
	found := d.store().Find(d.acct(), want)
	for i, p := range paths {
		if i == at {
			start = len(songs)
		}
		switch id := ids[i]; {
		case id != "":
			var sg subsonic.Song
			if s, ok := found[id]; ok {
				sg = s.Subsonic()
			} else if sg, err = cl.Song(ctx, id); errors.Is(err, subsonic.ErrNotFound) && i != at {
				continue // gone from the server: the list plays on without it
			} else if err != nil {
				return nil, nil, 0, fmt.Errorf("song %s: %w", id, err)
			}
			songs, local = append(songs, sg), append(local, keep[i])
		case filepath.Clean(p) == p && strings.HasPrefix(p, d.card+"/"): // a card file of the list
			songs = append(songs, subsonic.Song{Title: strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))})
			local = append(local, p)
		case i == at:
			return nil, nil, 0, fmt.Errorf("the song restored, %q, is not on the card", p)
		}
	}
	return songs, local, start, nil
}
