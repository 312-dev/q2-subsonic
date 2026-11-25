// Package pace obeys the firmware's Bluetooth coexistence state (Q2 Pod's docs/internals.md, "Bluetooth
// coexistence pacing"). The Q2's Wi-Fi and Bluetooth share one 2.4 GHz radio, and a download received at 1.5 to
// 2 MB/s on 2.4 GHz while A2DP streamed broke the headset's music up (2026-11-24). So while a stream runs with Wi-Fi
// joined on 2.4 GHz, q2guard writes /tmp/q2-bt-coex, "paced=1 rate=204800 freq=2412", and the app holds its
// background downloads to rate bytes a second; "paced=0", or no file at all (firmware without it), holds nothing.
package pace

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

// File is the state's path on the Q2.
const File = "/tmp/q2-bt-coex"

// maxRate is the most a rate may say: a larger one is not q2guard's.
const maxRate = 100_000_000

// State is the file's word: whether to pace, and to how many bytes a second.
type State struct {
	Paced bool
	Rate  int64
}

// Parse reads the file's line: paced only with paced=1 and a rate of 1 to maxRate.
func Parse(b []byte) State {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Split(bufio.ScanWords)
	var paced bool
	var rate int64
	for sc.Scan() {
		k, v, _ := bytes.Cut(sc.Bytes(), []byte("="))
		switch string(k) {
		case "paced":
			paced = string(v) == "1"
		case "rate":
			rate, _ = strconv.ParseInt(string(v), 10, 64)
		}
	}
	if !paced || rate <= 0 || rate > maxRate {
		return State{}
	}
	return State{Paced: true, Rate: rate}
}

// Gate reads the state file at most every Every, and holds readers to its rate.
type Gate struct {
	Path  string
	Every time.Duration
	Now   func() time.Time
	// Sleep waits d, or until ctx ends (its error then).
	Sleep func(ctx context.Context, d time.Duration) error
	// Changed, optional, is told of each new state once read (log.txt's line).
	Changed func(State)

	mu   sync.Mutex
	at   time.Time
	read bool
	st   State
}

// New is a gate on path, read again each second.
func New(path string) *Gate {
	return &Gate{Path: path, Every: time.Second, Now: time.Now, Sleep: sleep}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// State is the file's state, read again once Every has passed since the last read. A nil gate is never paced.
func (g *Gate) State() State {
	if g == nil {
		return State{}
	}
	g.mu.Lock()
	now := g.Now()
	if g.read && now.Sub(g.at) < g.Every {
		st := g.st
		g.mu.Unlock()
		return st
	}
	b, _ := os.ReadFile(g.Path)
	st, was, first := Parse(b), g.st, !g.read
	g.st, g.at, g.read = st, now, true
	g.mu.Unlock()
	if g.Changed != nil && st != was && !(first && !st.Paced) {
		g.Changed(st)
	}
	return st
}

// Paced is whether background work is to be held now.
func (g *Gate) Paced() bool { return g.State().Paced }

// Reader is r held to the gate's rate while it is paced and free (nil: never) says not. It counts the bytes of
// each second and sleeps off any lead after a read, each read cut to a tenth of a second's worth, so the
// socket's receive window holds the server back rather than a burst arriving at full speed. A free read, or one
// while unpaced, starts the count afresh.
func (g *Gate) Reader(ctx context.Context, r io.Reader, free func() bool) io.Reader {
	if g == nil {
		return r
	}
	return &reader{g: g, ctx: ctx, r: r, free: free}
}

type reader struct {
	g     *Gate
	ctx   context.Context
	r     io.Reader
	free  func() bool
	start time.Time // this second's start, zero when not counting
	n     int64     // its bytes
}

func (p *reader) Read(b []byte) (int, error) {
	st := p.g.State()
	if !st.Paced || p.free != nil && p.free() {
		p.start = time.Time{}
		return p.r.Read(b)
	}
	if most := int(st.Rate / 10); most > 0 && len(b) > most {
		b = b[:most]
	}
	k, err := p.r.Read(b)
	now := p.g.Now()
	if p.start.IsZero() || now.Sub(p.start) >= time.Second {
		p.start, p.n = now, 0
	}
	p.n += int64(k)
	due := time.Duration(p.n * int64(time.Second) / st.Rate)
	if wait := due - now.Sub(p.start); wait > 0 {
		if serr := p.g.Sleep(p.ctx, wait); serr != nil && err == nil {
			err = serr
		}
	}
	return k, err
}
