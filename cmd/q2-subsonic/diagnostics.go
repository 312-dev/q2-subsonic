package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Settings > Diagnostics (the user's decision, 2026-11-19 22:37): what went wrong, and the syncs,
// newest first, each a two-line row (what; why · when) that opens its Details in plain words. It
// replaces the loose "Skipped …" rows the first page showed. Events are kept in
// .subsonic/diagnostics.json for diagKeep, diagMax at most.

const (
	diagKeep = 7 * 24 * time.Hour
	diagMax  = 100
)

// event is one line of Diagnostics.
type event struct {
	At      time.Time `json:"at"`
	What    string    `json:"what"`              // "Skipped Mama, I'm Coming Home", "Server out of reach", "Synced 12 songs"
	Why     string    `json:"why,omitempty"`     // "Server error 502", "380 MB"
	Problem bool      `json:"problem,omitempty"` // a failure: its row's mark, Settings' subtitle
	Op      string    `json:"op,omitempty"`      // what the app was doing: stream, sync, ping
	HTTP    int       `json:"http,omitempty"`
	Code    int       `json:"code,omitempty"`
	Reach   string    `json:"reach,omitempty"` // reachCode's: 502, none, dns
	Song    string    `json:"song,omitempty"`  // a song's title
	Tries   int       `json:"tries,omitempty"`
	Error   string    `json:"error,omitempty"` // the error's text, redacted, for a failure nothing else explains
}

type diagState struct {
	mu sync.Mutex
}

func (d *daemon) diagPath() string { return filepath.Join(d.dir, "diagnostics.json") }

func (d *daemon) diagLoad() []event {
	var evs []event
	if b, err := os.ReadFile(d.diagPath()); err == nil {
		json.Unmarshal(b, &evs)
	}
	return evs
}

// diagAdd records e, newest first, dropping events past diagKeep and diagMax.
func (d *daemon) diagAdd(e event) {
	d.diag.mu.Lock()
	defer d.diag.mu.Unlock()
	if e.At.IsZero() {
		e.At = time.Now()
	}
	evs := append([]event{e}, d.diagLoad()...)
	evs = slices.DeleteFunc(evs, func(x event) bool { return time.Since(x.At) > diagKeep })
	if len(evs) > diagMax {
		evs = evs[:diagMax]
	}
	b, _ := json.Marshal(evs)
	if err := library.WriteOwned(d.card, d.diagPath(), b); err != nil {
		d.log.Printf("diagnostics: %v", err)
	}
}

// failure is an event for err: its why in words and its codes.
func failure(what, op string, err error) event {
	e := event{What: what, Op: op, Problem: true, Reach: reachCode(err)}
	var ae *subsonic.APIError
	if errors.As(err, &ae) {
		e.HTTP, e.Code = ae.HTTP, ae.Code
	}
	switch {
	case e.Reach == "none":
		e.Why = "Server not answering"
	case e.Reach == "dns":
		e.Why = "Server not found"
	case e.Reach != "":
		e.Why = "Server error " + e.Reach
	case e.HTTP >= 500:
		e.Why = "Server error " + strconv.Itoa(e.HTTP)
	case e.Code == 70:
		e.Why = "Not on the server"
	case err != nil:
		e.Why = subsonic.Message(err, "the server")
	}
	if err != nil && e.HTTP == 0 && e.Code == 0 && e.Reach == "" {
		e.Error = err.Error()
	}
	return e
}

// diagnostics is the page: two-line rows, newest first; the empty state when there are none.
func (d *daemon) diagnostics(cursor string) q2spot.Reply {
	evs := d.diagLoad()
	if len(evs) == 0 {
		if d.caps.Status {
			return status("clock-counter-clockwise", "Nothing Yet", []string{"Syncs and problems show up here"}, -1, "", "", "", false)
		}
		return q2spot.Reply{Rows: []q2spot.Row{{Kind: "info", Title: "Nothing yet", Subtitle: "Syncs and problems show up here"}}}
	}
	now := time.Now()
	var rows []q2spot.Row
	for i, e := range evs {
		sub := eventTime(e.At, now)
		if e.Why != "" {
			sub = e.Why + " · " + sub
		}
		rows = append(rows, list("event", e.What, sub, strconv.Itoa(i)))
	}
	return d.page(rows, cursor)
}

// eventTime is an event's time: "18:01" today, "Mon" this week, else "Nov 2".
func eventTime(t, now time.Time) string {
	y, m, dd := now.Date()
	today := time.Date(y, m, dd, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return t.Format("15:04")
	case !t.Before(today.AddDate(0, 0, -6)):
		return t.Format("Mon")
	}
	return t.Format("Jan 2")
}

// diagLatest is Settings' Diagnostics subtitle: the latest problem, "Server error 502 · 18:01".
func (d *daemon) diagLatest() string {
	for _, e := range d.diagLoad() {
		if e.Problem {
			return strings.TrimPrefix(e.Why+" · "+eventTime(e.At, time.Now()), " · ")
		}
	}
	return ""
}

// eventDetails is one event's Details (eventPage's rows).
func (d *daemon) eventPage(arg string) q2spot.Reply {
	i, err := strconv.Atoi(arg)
	evs := d.diagLoad()
	if err != nil || i < 0 || i >= len(evs) {
		return q2spot.Reply{Rows: []q2spot.Row{info("No longer kept")}}
	}
	return detailRows(evs[i], d.hostName())
}

// opWords is what the app was doing, in words.
var opWords = map[string]string{
	"stream": "Streaming a song", "download": "Downloading a song", "sync": "Syncing", "ping": "Asking the server",
	"migrate": "Moving Navidrome/ to Subsonic/", "library": "Updating the Library", "getAlbum": "Reading an album",
	"getPlaylist": "Reading a playlist", "getArtist": "Reading an artist", "search3": "Searching",
}

// sentence is why, said so a person knows what to do.
func sentence(e event) string {
	switch {
	case e.Reach == "502" || e.Reach == "503" || e.Reach == "504":
		return "The proxy in front of your server answered, but the server behind it didn't: it may be stopped."
	case e.Reach == "none":
		return "The server didn't answer: it may be off, or the Q2 can't reach it on this Wi-Fi."
	case e.Reach == "dns":
		return "The server's name didn't lead anywhere: check the address, or the Wi-Fi's DNS."
	case e.HTTP >= 500:
		return "The server answered with an error of its own."
	case e.Code == 70:
		return "The server no longer has it."
	case e.Code == 40 || e.Code == 41 || e.Code == 44:
		return "The server didn't accept the sign-in: sign in again."
	}
	return ""
}

// detailRows is Details in plain words (the user's decision, 2026-11-19: no "Op: stream"): a label
// and value a row, then a sentence saying what it means.
func detailRows(e event, host string) q2spot.Reply {
	rows := []q2spot.Row{{Kind: "info", Title: "What", Subtitle: e.What}}
	if e.Op != "" {
		if w, ok := opWords[e.Op]; ok {
			rows = append(rows, q2spot.Row{Kind: "info", Title: "While", Subtitle: w})
		}
	}
	if e.Why != "" {
		rows = append(rows, q2spot.Row{Kind: "info", Title: "Why", Subtitle: e.Why})
	}
	rows = append(rows, q2spot.Row{Kind: "info", Title: "When", Subtitle: whenAt(e.At, time.Now())})
	if e.Song != "" {
		rows = append(rows, q2spot.Row{Kind: "info", Title: "Song", Subtitle: e.Song})
	}
	if host != "" {
		rows = append(rows, q2spot.Row{Kind: "info", Title: "Server", Subtitle: host})
	}
	if e.Tries > 0 {
		rows = append(rows, q2spot.Row{Kind: "info", Title: "Tries", Subtitle: strconv.Itoa(e.Tries)})
	}
	if e.Error != "" {
		rows = append(rows, q2spot.Row{Kind: "info", Title: "Error", Subtitle: e.Error})
	}
	if s := sentence(e); s != "" {
		rows = append(rows, info(s))
	}
	return q2spot.Reply{Rows: rows}
}

// details is the Details page of a context the other pages build (detailsCtx: op|http|code|unix|
// song|error), in the same plain words as an event's.
func details(ctx string) q2spot.Reply {
	f := strings.SplitN(ctx, "|", 6)
	for len(f) < 6 {
		f = append(f, "")
	}
	unix, _ := strconv.ParseInt(f[3], 10, 64)
	h, _ := strconv.Atoi(f[1])
	code, _ := strconv.Atoi(f[2])
	e := event{At: time.Unix(unix, 0), Op: f[0], HTTP: h, Code: code, Song: f[4], Error: f[5], What: "Something failed"}
	if w, ok := opWords[f[0]]; ok {
		e.What = w + " failed"
	}
	if h == 502 || h == 503 || h == 504 {
		e.Reach = f[1]
	}
	switch {
	case e.Reach != "":
		e.Why = "Server error " + e.Reach
	case h >= 500:
		e.Why = fmt.Sprintf("Server error %d", h)
	case code == 70:
		e.Why = "Not on the server"
	}
	return detailRows(e, "")
}
