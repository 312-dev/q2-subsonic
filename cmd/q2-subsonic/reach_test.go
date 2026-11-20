package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

func TestReachCode(t *testing.T) {
	api := func(h, code int, kind error) error {
		e := &subsonic.APIError{Op: "stream", HTTP: h, Code: code}
		if kind != nil {
			return fmt.Errorf("%w: %w", e, kind)
		}
		return e
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{api(502, 0, subsonic.ErrUnreachable), "502"},
		{api(503, 0, subsonic.ErrUnreachable), "503"},
		{api(504, 0, subsonic.ErrUnreachable), "504"},
		{api(0, 0, subsonic.ErrUnreachable), "none"},
		{api(0, 0, subsonic.ErrStalled), "none"},
		{api(0, 0, subsonic.ErrNoName), "dns"},
		{api(500, 0, subsonic.ErrUnreachable), ""}, // the server itself answered
		{api(404, 0, subsonic.ErrUnreachable), ""}, // an endpoint it lacks
		{api(200, 70, subsonic.ErrNotFound), ""},
		{api(200, 40, subsonic.ErrAuth), ""},
		{errors.New("card full"), ""},
	} {
		if got := reachCode(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}

// A proxy answering 502 for a server that is down marks the server out of reach and toasts once;
// more failures, of the same kind or another, toast nothing more; the first answer that serves takes the mark away and leaves
// the last toast's key as it was (the firmware has shown it).
func TestReachMarksAndClears(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	cl := d.client()
	ctx := context.Background()
	if err := cl.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if v := d.stateKey("reach"); v != "" {
		t.Fatalf("reach=%q with the server up", v)
	}
	n.proxy.Store(502)
	if cl.Ping(ctx) == nil {
		t.Fatal("ping served through a 502")
	}
	if v := d.stateKey("reach"); v != "down 502" {
		t.Fatalf("reach=%q, want down 502", v)
	}
	toast := d.stateKey("srverr")
	at, code, _ := strings.Cut(toast, " ")
	if code != "502" || at == "" || strings.Trim(at, "0123456789") != "" {
		t.Fatalf("srverr=%q, want <unix> 502", toast)
	}
	d.state.setReach(d.state.reachNow(), "1 marker") // a second toast would overwrite this
	cl.Ping(ctx)
	n.proxy.Store(504) // the proxy's word changes: the mark follows, with no new toast
	cl.Ping(ctx)
	if v := d.stateKey("srverr"); v != "1 marker" || d.stateKey("reach") != "down 504" {
		t.Fatalf("srverr=%q reach=%q: toasted again while still down", v, d.stateKey("reach"))
	}
	n.proxy.Store(0)
	if err := cl.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if v := d.stateKey("reach"); v != "" {
		t.Fatalf("reach=%q after the server answered", v)
	}
	if v := d.stateKey("srverr"); v != "1 marker" {
		t.Fatalf("srverr=%q changed on the way back", v)
	}
}

// An answer from the server itself, even a failure, neither marks nor clears: only a request that
// succeeds clears the mark (the user's rule, 2026-11-19).
func TestReachServerAnswersKeepTheMark(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	cl := d.client()
	n.errCode = 70
	cl.Ping(context.Background())
	if v := d.stateKey("reach"); v != "" {
		t.Fatalf("reach=%q for a Subsonic error", v)
	}
	n.errCode = 0
	n.proxy.Store(504)
	cl.Ping(context.Background())
	n.proxy.Store(0)
	n.errCode = 70
	cl.Ping(context.Background())
	if v := d.stateKey("reach"); v != "down 504" {
		t.Fatalf("reach=%q: a failed answer cleared the mark", v)
	}
}

// No answer at all is "none"; a request its caller gave up says nothing.
func TestReachNoAnswerAndCancelled(t *testing.T) {
	n := &nav{stall: true}
	d := newTestDaemon(t, n, "", true)
	cl := d.client()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cl.Ping(ctx)
	if v := d.stateKey("reach"); v != "" {
		t.Fatalf("reach=%q for a request its caller cancelled", v)
	}
	cl.Within(50 * time.Millisecond).Ping(context.Background())
	if v := d.stateKey("reach"); v != "down none" {
		t.Fatalf("reach=%q for no answer, want down none", v)
	}
}

// A streamed song given up toasts why when the server is why: a 5xx of its own too, which leaves
// the reach mark alone; a song the server says is gone toasts nothing.
func TestReachGaveUpToasts(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	d.reach.now = func() time.Time { return time.Unix(1700000000, 0) }
	d.reach.gaveUp(&subsonic.APIError{Op: "stream", HTTP: 200, Code: 70})
	if v := d.stateKey("srverr"); v != "" {
		t.Fatalf("srverr=%q for a song not found", v)
	}
	d.reach.gaveUp(fmt.Errorf("%w: %w", &subsonic.APIError{Op: "stream", HTTP: 500}, subsonic.ErrUnreachable))
	if v, r := d.stateKey("srverr"), d.stateKey("reach"); v != "1700000000 500" || r != "" {
		t.Fatalf("srverr=%q reach=%q, want a 500 toast and no mark", v, r)
	}
	d.reach.now = func() time.Time { return time.Unix(1700000040, 0) }
	d.reach.gaveUp(fmt.Errorf("%w: %w", &subsonic.APIError{Op: "stream", HTTP: 502}, subsonic.ErrUnreachable))
	if v, r := d.stateKey("srverr"), d.stateKey("reach"); v != "1700000040 502" || r != "down 502" {
		t.Fatalf("srverr=%q reach=%q", v, r)
	}
}

// Sign Out takes the mark away: there is no server to reach.
func TestReachSignOutClears(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "", true)
	n.proxy.Store(502)
	d.client().Ping(context.Background())
	d.signOut()
	if v := d.stateKey("reach"); v != "" {
		t.Fatalf("reach=%q after Sign Out", v)
	}
}
