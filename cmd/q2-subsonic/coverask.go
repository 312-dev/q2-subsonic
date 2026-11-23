package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/catalogue"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Covers the car asks for (Q2 Muse's Car Remote, its docs/internals.md#car-remote): when the car
// wants a server album's cover the Q2 has none of (no covers/<album>.jpg, no picture in a streamed
// song of it), the firmware writes /tmp/q2-subsonic.coverask, a line per cover, "<seq> <px> <album
// id>", px 512 for the car's covers/512/ (catalogue.BigName) or 160 for covers/, and starts the app
// if it doesn't run. The app fetches each line it has not read before at once, one at a time and
// ahead of a sync's next file, saves it where the firmware's GetCover reads it, and answers each,
// in the file's order, in its state file: coverack=<seq> <code>. A line there when the app starts is
// new to it: the firmware drops a line once it is answered. An album the server has no cover for is
// noted in the folder's .none, as a round notes it, and not asked again; a fetch that failed (the
// server out of reach, no room on the card) is not asked again for coverFailFor. The minute's
// rounds (slCovers) are untouched, and a cover saved while they were settled leaves them settled.
//
// Car Audio's q2-iap asks the same way through .coverask-iap, its own seq space, answered in
// coverack-iap= (coverAskIapPath); its fetches take no network slot.

// Answers to a .coverask line, in the state file's coverack=<seq> <code>.
const (
	coverSaved   = 0 // the file is there: the firmware sends it
	coverNotOurs = 1 // not one of the app's: a bad line, a size it keeps none at, signed out
	coverNone    = 2 // the server has none for the album (noted in .none)
	coverFailed  = 3 // not fetched: signed out, the server out of reach, or no room on the card
)

// coverFailFor is how long a cover whose fetch failed is answered as failed without asking.
const coverFailFor = 10 * time.Minute

// coverAskIn is the server's time to send one asked-for cover, the network slot's wait included.
var coverAskIn = 20 * time.Second

type coverAskState struct {
	mu      sync.Mutex
	raw     string               // the request file as last read
	seen    map[string]bool      // its lines
	queue   []string             // lines still to answer, in order
	running bool                 // a goroutine is answering queue
	failed  map[string]time.Time // "<account> <px> <album id>": when its fetch failed
}

func (d *daemon) coverAskPath() string { return filepath.Join(d.tmp, "q2-"+appID+".coverask") }

// coverAskIapPath is Car Audio's ask file: q2-iap's, the car on the USB cable (q2-coda's
// patch/q2_iap.c), the same lines as .coverask in a seq space of its own, answered in the state
// file's coverack-iap=<seq> <code>, the cover saved where .coverask's go. Its fetches take no
// network slot (slotNone): the car waits on its cover while a song downloads.
func (d *daemon) coverAskIapPath() string { return filepath.Join(d.tmp, "q2-"+appID+".coverask-iap") }

// coverSource is one ask file: where it is, its state, how its answers are written, its slot.
type coverSource struct {
	path func() string
	st   *coverAskState
	ack  func(string) error
	slot int
}

func (d *daemon) coverSources() []coverSource {
	return []coverSource{{d.coverAskPath, &d.cov, d.state.setCoverAck, slotFirst},
		{d.coverAskIapPath, &d.covIap, d.state.setCoverAckIap, slotNone}}
}

// coverTick queues each request file's new lines and starts answering them in the background.
func (d *daemon) coverTick() {
	for _, src := range d.coverSources() {
		d.coverTickFrom(src)
	}
}

func (d *daemon) coverTickFrom(src coverSource) {
	b, err := os.ReadFile(src.path())
	if err != nil {
		return
	}
	st := src.st
	st.mu.Lock()
	defer st.mu.Unlock()
	if string(b) == st.raw && st.seen != nil {
		return
	}
	st.raw = string(b)
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		if !st.seen[l] {
			st.queue = append(st.queue, l)
		}
	}
	st.seen = seen
	if len(st.queue) > 0 && !st.running {
		st.running = true
		d.spawn(func() { d.coverRun(src) })
	}
}

// coverRun answers src's queued lines, one at a time, until none is left.
func (d *daemon) coverRun(src coverSource) {
	st := src.st
	for {
		st.mu.Lock()
		if len(st.queue) == 0 {
			st.running = false
			st.mu.Unlock()
			return
		}
		line := st.queue[0]
		st.queue = st.queue[1:]
		st.mu.Unlock()
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if _, err := strconv.ParseUint(f[0], 10, 64); err != nil {
			d.log.Printf("covers: bad request %q", line)
			continue
		}
		code := coverNotOurs
		if len(f) == 3 {
			code = d.coverFor(st, f[1], f[2], src.slot)
		}
		src.ack(f[0] + " " + strconv.Itoa(code))
	}
}

// coverFor puts album id's cover at px where the firmware reads it, and says how that went; st's
// failures are remembered, and slot is how its request takes the network slot (fetchCover).
func (d *daemon) coverFor(st *coverAskState, px, id string, slot int) int {
	var cv catalogue.Covers
	switch px {
	case strconv.Itoa(catalogue.BigPx):
		cv = d.bigCovers()
	case strconv.Itoa(catalogue.CoverPx):
		cv = d.covers()
	default:
		return coverNotOurs
	}
	if !catalogue.CoverID(id) || !d.slActive() {
		return coverNotOurs
	}
	if exists(cv.Path(id)) {
		return coverSaved
	}
	cl, acct := d.client(), d.acct()
	if cl == nil || acct == "" {
		return coverFailed
	}
	key := acct + " " + px + " " + id
	st.mu.Lock()
	at, failed := st.failed[key]
	st.mu.Unlock()
	if failed && time.Since(at) < coverFailFor {
		return coverFailed
	}
	if cv.Noted(id) {
		return coverNone
	}
	fail := func(err error) int {
		d.log.Printf("covers: the car's %s (%s px): %s", id, px, applog.Redact(err.Error()))
		st.mu.Lock()
		if st.failed == nil {
			st.failed = map[string]time.Time{}
		}
		for k, at := range st.failed {
			if time.Since(at) >= coverFailFor {
				delete(st.failed, k)
			}
		}
		st.failed[key] = time.Now()
		st.mu.Unlock()
		return coverFailed
	}
	before := d.coversKey()
	d.sl.mu.Lock()
	settled := d.sl.settled != "" && d.sl.settled == before
	d.sl.mu.Unlock()
	pass, err := cv.Open()
	if err != nil {
		return fail(err)
	}
	if settled { // what this adds to the folders gives the rounds nothing more to fetch
		defer func() {
			after := d.coversKey()
			d.sl.mu.Lock()
			if d.sl.settled == before {
				d.sl.settled = after
			}
			d.sl.mu.Unlock()
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), coverAskIn)
	defer cancel()
	jpg, err := d.fetchCover(ctx, cl, id, cv, slot)
	var ae *subsonic.APIError
	switch {
	case err == errNoCover || errors.As(err, &ae) && (ae.HTTP == 404 || ae.Code == 70):
		if err := pass.None(id); err != nil {
			d.log.Printf("covers: %v", err)
		}
		return coverNone
	case err != nil:
		if subsonic.ConnectionLost(err) {
			d.slServer(err)
		}
		return fail(err)
	}
	if err := pass.Save(id, jpg); err != nil {
		return fail(err)
	}
	d.log.Printf("covers: the car's %s (%s px) saved, %d KiB", id, px, len(jpg)>>10)
	d.stWrote()
	return coverSaved
}
