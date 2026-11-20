package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

// Sync as one centred stack per state (Q2 Muse's docs/oled-design.md#sync, the user's 2026-11-19
// approval of mock_sync_apple.py), on firmware with the "status" word: a reply led by an info row
// "status" (its context the glyph, its title, its subtitle's lines, its position the bar's permille),
// the first action row its button and an info row "foot" its footer.

// reopenAfter is how long after a check was shown a request for Sync is a new opening of the page,
// which asks the server again: the firmware asks again at once after an action or Back, never on
// its own for a page that isn't live.
const reopenAfter = 3 * time.Second

// status is one state's reply.
func status(glyph, title string, lines []string, bar int, button, op, foot string, live bool) q2spot.Reply {
	head := q2spot.Row{Kind: "info", URI: "status", Title: title, Subtitle: strings.Join(lines, "\n"), Context: glyph}
	if bar >= 0 {
		head.Position = strconv.Itoa(min(bar, 1000))
	}
	rows := []q2spot.Row{head}
	if button != "" {
		rows = append(rows, act(op, button, ""))
	}
	if foot != "" {
		rows = append(rows, q2spot.Row{Kind: "info", URI: "foot", Title: foot})
	}
	r := q2spot.Reply{Rows: rows}
	if live {
		r.Message = "refresh"
	}
	return r
}

// whenAt is when, as the footer says it: "today at 18:01", "yesterday at 21:40", "Nov 2 at 15:04".
func whenAt(t, now time.Time) string {
	w := when(t, now)
	if i := strings.LastIndex(w, " "); i > 0 {
		return w[:i] + " at " + w[i+1:]
	}
	return w
}

// lastSyncedFoot is the footer's "Last synced <when>", or "" before any sync.
func (d *daemon) lastSyncedFoot() string {
	if at := d.lastSynced(); !at.IsZero() {
		return "Last synced " + whenAt(at, time.Now())
	}
	return ""
}

// reason is why the server couldn't be reached, as a sentence: "Server error 502.".
func (d *daemon) reason(err error) string {
	switch code := reachCode(err); code {
	case "none":
		return "Server not answering."
	case "dns":
		return "Server not found."
	case "":
		return strings.TrimSuffix(d.stopMessage(err), ".") + "."
	default:
		return "Server error " + code + "."
	}
}

func (d *daemon) syncingStatus(s syncer.Snapshot) q2spot.Reply {
	if s.State == "paused" {
		why := "Paused."
		if s.Err != nil {
			why = d.reason(s.Err)
		}
		return status("arrow-circle-down", "Sync paused", []string{fmt.Sprintf("%d of %s done.", s.Done, plural(s.Total, "song")), why},
			s.Permille, "Resume", "resume", d.lastSyncedFoot(), true)
	}
	n := min(s.Done+1, s.Total)
	var lines []string
	if artist, title, ok := strings.Cut(s.Current, " – "); ok {
		lines = append(lines, title+" · "+artist)
	} else if s.Current != "" {
		lines = append(lines, s.Current)
	}
	return status("arrow-circle-down", fmt.Sprintf("Syncing %d of %d", n, s.Total), lines, s.Permille, "Stop", "cancel",
		"Keep the Q2 on Wi-Fi until it finishes", true)
}

func (d *daemon) doneStatus(s syncer.Snapshot) q2spot.Reply {
	lines := []string{plural(s.Done, "song") + " synced."}
	if s.Removed > 0 {
		lines = append(lines, plural(s.Removed, "song")+" removed.")
	}
	if s.Failed > 0 {
		lines = append(lines, plural(s.Failed, "song")+" failed.")
		return status("check-circle", "Sync complete", lines, -1, "Retry Failed", "retry", d.lastSyncedFoot(), false)
	}
	return status("check-circle", "Sync complete", lines, -1, "", "", d.lastSyncedFoot(), false)
}

func (d *daemon) stoppedStatus(s syncer.Snapshot) q2spot.Reply {
	why := "Sync stopped."
	if s.Err != nil {
		why = d.reason(s.Err)
	}
	return status("cloud-slash", "Sync stopped", []string{why, "Offline songs still play."}, -1, "Try Again", "retry",
		d.lastSyncedFoot(), false)
}

func (d *daemon) checkingStatus() q2spot.Reply {
	return status("arrows-clockwise", "Checking for changes…", []string{"Asking " + d.hostName()}, -1, "", "",
		d.lastSyncedFoot(), true)
}

func (d *daemon) failedStatus(err error) q2spot.Reply {
	return status("cloud-slash", "Couldn't reach the server", []string{d.reason(err), "Offline songs still play."}, -1,
		"Try Again", "check", d.lastSyncedFoot(), false)
}

// previewStatus is a finished check: nothing chosen, up to date, or what a sync will do.
func (d *daemon) previewStatus(p *plan.Plan, chosen int, at time.Time, reclaim, over, limit int64) q2spot.Reply {
	now := time.Now()
	foot := d.lastSyncedFoot()
	if !changes(*p) {
		if chosen == 0 {
			return status("cloud", "Nothing to sync yet", []string{"Hold a song, album or artist", "and choose Sync Offline."},
				-1, "", "", "", false)
		}
		checked := "Checked just now"
		if now.Sub(at) >= time.Minute {
			checked = "Checked " + whenAt(at, now)
		}
		if foot != "" {
			foot = checked + " · last synced " + strings.TrimPrefix(foot, "Last synced ")
		} else {
			foot = checked
		}
		return status("check-circle", "Up to date", []string{"Your offline songs match the server."}, -1, "", "", foot, false)
	}
	var addB int64
	for _, w := range p.Add {
		addB += w.Bytes
	}
	title := plural(len(p.Add), "new song")
	switch {
	case len(p.Add) == 0 && len(p.Remove) > 0:
		title = plural(len(p.Remove), "song") + " to remove"
	case len(p.Add) == 0:
		title = "Changes to sync"
	}
	free := p.FreeBytes - reclaim
	var lines []string
	if len(p.Add) > 0 {
		lines = append(lines, size(addB)+" · "+size(free)+" free")
	}
	if len(p.Remove) > 0 && len(p.Add) > 0 {
		lines = append(lines, plural(len(p.Remove), "song")+" will be removed")
	}
	if len(p.Replace) > 0 || len(p.Rename) > 0 || p.PlaylistsChanged > 0 || p.PlaylistsStale {
		if len(lines) == 0 {
			lines = append(lines, "Songs and playlists to update")
		}
	}
	switch {
	case over > 0:
		lines = append(lines, "Over the "+size(limit)+" Downloads limit")
		return status("arrow-circle-down", title, lines, -1, "", "", foot, false)
	case !p.Fits:
		lines = append(lines, "Not enough space: free "+size(p.NeedBytes+planMargin-p.FreeBytes))
		return status("arrow-circle-down", title, lines, -1, "", "", foot, false)
	}
	return status("arrow-circle-down", title, lines, -1, "Sync Now", "start", foot, false)
}
