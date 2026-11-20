package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Diagnostics with nothing in it is the centred empty state on firmware with "status"; the server
// going out of reach is a row with why, and it opens Details that say what it means.
func TestDiagnosticsServerOutOfReach(t *testing.T) {
	n := &nav{}
	d := newTestDaemon(t, n, "status", true)
	if g, title, lines, _, _ := head(t, d.do("diagnostics")); g != "clock-counter-clockwise" || title != "Nothing Yet" ||
		strings.Join(lines, "|") != "Syncs and problems show up here" {
		t.Fatalf("empty: %q %q %q", g, title, lines)
	}
	n.proxy.Store(502)
	d.client().Ping(context.Background())
	d.client().Ping(context.Background()) // still down: one event
	r := d.do("diagnostics")
	if len(r.Rows) != 1 || r.Rows[0].Title != "Server out of reach" || !strings.HasPrefix(r.Rows[0].Subtitle, "Server error 502 · ") {
		t.Fatalf("rows %+v", r.Rows)
	}
	got := strings.Join(pairs(d.do(r.Rows[0].URI, r.Rows[0].Context)), "|")
	if !strings.Contains(got, "Why=Server error 502") || !strings.Contains(got, "it may be stopped") {
		t.Fatalf("details %q", got)
	}
}

// Events older than diagKeep go, and at most diagMax are kept, newest first.
func TestDiagnosticsKeepsAWeek(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "", true)
	d.diagAdd(event{What: "old", At: time.Now().Add(-diagKeep - time.Hour)})
	for i := 0; i < diagMax+5; i++ {
		d.diagAdd(event{What: "new"})
	}
	evs := d.diagLoad()
	if len(evs) != diagMax || evs[len(evs)-1].What != "new" {
		t.Fatalf("%d kept, last %q", len(evs), evs[len(evs)-1].What)
	}
}

// A finished sync is an event: how many songs, their size.
func TestDiagnosticsSync(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "", true)
	d.do("toggle", "album:al0")
	d.preview(t)
	d.do("start")
	eventually(t, "the sync", func() bool { _, ok := find(d.do("diagnostics"), "Synced 2 songs"); return ok })
}
