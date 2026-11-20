package main

import (
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

// head is a status reply's stack: glyph, title, lines; its button "title op", or ""; its footer.
func head(t *testing.T, r q2spot.Reply) (glyph, title string, lines []string, button, foot string) {
	t.Helper()
	if len(r.Rows) == 0 || r.Rows[0].Kind != "info" || r.Rows[0].URI != "status" {
		t.Fatalf("not a status reply: %+v", r.Rows)
	}
	h := r.Rows[0]
	for _, row := range r.Rows[1:] {
		if row.Kind == "action" && button == "" {
			button = row.Title + " " + row.URI
		}
		if row.Kind == "info" && row.URI == "foot" {
			foot = row.Title
		}
	}
	if h.Subtitle != "" {
		lines = strings.Split(h.Subtitle, "\n")
	}
	return h.Context, h.Title, lines, button, foot
}

// With "status", Sync opens on Checking, live, then nothing chosen is said as such, after the server
// was asked: "up to date" never comes without an answer (the user's report, 2026-11-19).
func TestSyncStatusNothingChosenAsksTheServer(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1}
	d := newTestDaemon(t, n, "status", true)
	r := d.do("sync")
	if g, title, _, _, _ := head(t, r); g != "arrows-clockwise" || title != "Checking for changes…" || r.Message != "refresh" {
		t.Fatalf("checking: %q %q %q", g, title, r.Message)
	}
	g, title, lines, button, foot := head(t, d.preview(t))
	if g != "cloud" || title != "Nothing to sync yet" || strings.Join(lines, "|") != "Hold a song, album or artist|and choose Sync Offline." ||
		button != "" || foot != "" {
		t.Fatalf("nothing chosen: %q %q %q %q %q", g, title, lines, button, foot)
	}
	if n.reqs("ping") == 0 {
		t.Fatal("the server was not asked")
	}
}

// A server out of reach, even with nothing chosen, is the failure with its reason in words and
// Try Again, which checks again.
func TestSyncStatusFailure(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1}
	d := newTestDaemon(t, n, "status", true)
	n.proxy.Store(502)
	g, title, lines, button, _ := head(t, d.preview(t))
	if g != "cloud-slash" || title != "Couldn't reach the server" ||
		strings.Join(lines, "|") != "Server error 502.|Offline songs still play." || button != "Try Again check" {
		t.Fatalf("failure: %q %q %q %q", g, title, lines, button)
	}
}

// Changes: how many new songs, their size and the card's free space, and Sync Now.
func TestSyncStatusChanges(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 3}, "status", true)
	d.do("toggle", "album:al0")
	g, title, lines, button, _ := head(t, d.preview(t))
	if g != "arrow-circle-down" || title != "3 new songs" || len(lines) != 1 || !strings.HasSuffix(lines[0], " free") ||
		button != "Sync Now start" {
		t.Fatalf("changes: %q %q %q %q", g, title, lines, button)
	}
}

// Opened again after reopenAfter, Sync asks the server again; asked again at once (after an action
// or Back), it shows the check it has.
func TestSyncStatusAsksEachOpening(t *testing.T) {
	n := &nav{albums: 1, songsPer: 1}
	d := newTestDaemon(t, n, "status", true)
	d.preview(t)
	pings := n.reqs("ping")
	if _, title, _, _, _ := head(t, d.do("sync")); title != "Nothing to sync yet" {
		t.Fatalf("at once: %q", title)
	}
	d.mu.Lock()
	d.daemon.preview.shown = time.Now().Add(-2 * reopenAfter)
	d.mu.Unlock()
	d.preview(t)
	if n.reqs("ping") <= pings {
		t.Fatal("not asked again on a new opening")
	}
}

// Up to date says when it checked and when it last synced; syncing shows i of n, its bar, the song
// and Stop; done says what it did.
func TestSyncStatusStates(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "status", true)
	g, title, lines, button, foot := head(t, d.previewStatus(&plan.Plan{Fits: true}, 1, time.Now(), 0, 0, 0))
	if g != "check-circle" || title != "Up to date" || strings.Join(lines, "|") != "Your offline songs match the server." ||
		button != "" || !strings.HasPrefix(foot, "Checked just now") {
		t.Fatalf("up to date: %q %q %q %q %q", g, title, lines, button, foot)
	}
	r := d.syncingStatus(syncer.Snapshot{State: "running", Done: 3, Total: 12, Permille: 250, Current: "Post Malone – Rockstar"})
	g, title, lines, button, foot = head(t, r)
	if g != "arrow-circle-down" || title != "Syncing 4 of 12" || strings.Join(lines, "|") != "Rockstar · Post Malone" ||
		button != "Stop cancel" || foot != "Keep the Q2 on Wi-Fi until it finishes" || r.Rows[0].Position != "250" || r.Message != "refresh" {
		t.Fatalf("syncing: %q %q %q %q %q %+v", g, title, lines, button, foot, r)
	}
	_, title, lines, button, _ = head(t, d.doneStatus(syncer.Snapshot{State: "done", Done: 12, Failed: 1}))
	if title != "Sync complete" || strings.Join(lines, "|") != "12 songs synced.|1 song failed." || button != "Retry Failed retry" {
		t.Fatalf("done: %q %q %q", title, lines, button)
	}
	if got := whenAt(time.Date(2026, 11, 18, 21, 40, 0, 0, time.Local), time.Date(2026, 11, 19, 18, 1, 0, 0, time.Local)); got != "yesterday at 21:40" {
		t.Fatalf("whenAt %q", got)
	}
}

// Without the word, Sync keeps its rows, as on older firmware.
func TestSyncWithoutStatusKeepsRows(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	if r := d.preview(t); len(r.Rows) == 0 || r.Rows[0].URI == "status" {
		t.Fatalf("rows %+v", r.Rows)
	}
}
