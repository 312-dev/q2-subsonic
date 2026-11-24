package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// A song row's own menu, on firmware with "holdmenu" (q2-coda's docs/internals.md#card-apps): the
// row's hold op is "hold", which the firmware sends with the row's Context when Centre, a finger or
// Play/Pause is held on it, and the action rows this answers are its menu, in the order and words
// of the firmware's own song menu: Play Next and Play Last, when the song can be kept on the card,
// then Sync Offline or Stop Syncing Offline, unless Entire Library is chosen (as the firmware's
// Library leaves it out). Play Next and Play Last fetch the song into the Stream Cache, as a play
// does, and name it in /tmp/q2-subsonic.queue once it is whole; the firmware queues it from there.

// queueKeep is how many of the latest Play Next and Play Last lines .queue keeps, and how many of
// their songs the Stream Cache holds back from a trim or a clear, as it does the list playing.
const queueKeep = 8

// queueWait bounds a Play Next or Play Last song's fetch.
const queueWait = 10 * time.Minute

// holdMenu is the "hold" op's answer for a song row's Context ("song:<id> ...").
func (d *daemon) holdMenu(arg string) q2spot.Reply {
	kind, id := parseKey(arg)
	if kind != selection.KindSong || id == "" {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	var rows []q2spot.Row
	if d.keptSong(id) != "" || d.stream.cacheBase() != "" {
		rows = append(rows, act("next", "Play Next", arg), act("last", "Play Last", arg))
	}
	sel, err := d.selection()
	if err != nil {
		return q2spot.Fail(q2spot.Error, selDamaged)
	}
	if !sel.Has(selection.KindLibrary, "all") {
		title := "Sync Offline"
		if sel.Has(selection.KindSong, id) {
			title = "Stop Syncing Offline"
		}
		rows = append(rows, act("toggle", title, selection.Key(selection.KindSong, id)))
	}
	return q2spot.Reply{Rows: rows}
}

// keptSong is the card file song id plays from without a fetch: its synced copy, else the Stream
// Cache's (of any quality); "" when it has neither.
func (d *daemon) keptSong(id string) string {
	if e := d.manifest().Songs[id]; d.caps.Playlist && d.stream.synced(e) {
		return e.Path
	}
	return d.stream.copies()[id]
}

// enqueue is Play Next ("next") or Play Last ("last") on a song row's Context: a song on the card
// is named in .queue at once and the reply says nothing, as the firmware toasts what it did;
// another is fetched into the Stream Cache first, in the background, and the reply says so.
func (d *daemon) enqueue(ctx context.Context, cl *subsonic.Client, op, arg string) q2spot.Reply {
	kind, id := parseKey(arg)
	if kind != selection.KindSong || id == "" {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	if p := d.keptSong(id); p != "" {
		if err := d.queueLine(op, p); err != nil {
			d.log.Printf("queue: %v", err)
			return q2spot.Fail(q2spot.Error, "Couldn't queue the song")
		}
		return q2spot.Reply{}
	}
	songs, _, err := d.songsOf(ctx, cl, selection.KindSong, id)
	if err != nil {
		return d.errToast(err)
	}
	if len(songs) != 1 {
		return q2spot.Fail(q2spot.Unavailable, "Nothing to play")
	}
	sg := songs[0]
	j, ok := d.stream.keepJob(sg)
	if !ok {
		return q2spot.Reply{Message: "Turn on the Stream Cache to queue it"}
	}
	d.spawn(func() {
		fctx, cancel := context.WithTimeout(context.Background(), queueWait)
		defer cancel()
		if err := d.stream.fetchOne(fctx, sg, j); err != nil {
			d.log.Printf("queue: %q: %v", sg.Title, err)
			return
		}
		if err := d.queueLine(op, j.path); err != nil {
			d.log.Printf("queue: %v", err)
		}
	})
	if op == "next" {
		return q2spot.Reply{Message: "Playing Next once downloaded"}
	}
	return q2spot.Reply{Message: "Playing Last once downloaded"}
}

// queueLine adds "<seq> <op> <path>" to .queue, written whole (aside, then renamed), its latest
// queueKeep lines kept: the firmware queues each seq once.
func (d *daemon) queueLine(op, path string) error {
	if strings.ContainsAny(path, "\r\n") {
		return os.ErrInvalid
	}
	d.qmu.Lock()
	defer d.qmu.Unlock()
	d.queueSeq = max(d.queueSeq+1, time.Now().UnixNano())
	d.queued = append(d.queued, strconv.FormatInt(d.queueSeq, 10)+" "+op+" "+path+"\n")
	d.queued = d.queued[max(0, len(d.queued)-queueKeep):]
	d.stream.holdQueued(path)
	file := filepath.Join(d.tmp, "q2-subsonic.queue")
	if err := os.WriteFile(file+".tmp", []byte(strings.Join(d.queued, "")), 0o644); err != nil {
		return err
	}
	return os.Rename(file+".tmp", file)
}

// keepJob is how song sg is fetched to be kept: into the Stream Cache at Streaming Quality, whole
// before it is named (never in place); false when nothing is kept (the cache Off, signed out).
func (s *streamer) keepJob(sg subsonic.Song) (job, bool) {
	ext, q := s.format(sg, s.streamQuality())
	p := s.cachePath(s.cacheBase(), sg, ext, q)
	return job{path: p, fetch: true, quality: q, cached: true, whole: true}, p != ""
}

// fetchOne fetches song sg as j says, outside any list's run, held back from the cache's trim
// while it arrives and after.
func (s *streamer) fetchOne(ctx context.Context, sg subsonic.Song, j job) error {
	s.holdQueued(j.path)
	r := &streamRun{s: s, ctx: ctx, songs: []subsonic.Song{sg}, jobs: []job{j}, prog: progress{job: 0}}
	return r.fetch(ctx, sg, j)
}

// holdQueued keeps path among the songs Play Next and Play Last named last, which the cache never
// evicts, clears or sweeps (held).
func (s *streamer) holdQueued(path string) {
	s.kmu.Lock()
	defer s.kmu.Unlock()
	if !slices.Contains(s.queued, path) {
		s.queued = append(s.queued, path)
		s.queued = s.queued[max(0, len(s.queued)-queueKeep):]
	}
}
