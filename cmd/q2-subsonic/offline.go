package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Sync Offline from the Q2's Library (Q2 Muse's docs/internals.md#sync-offline): the firmware's hold
// menu on a server song, album, artist or playlist, and Now Playing's ⋯, offer Sync Offline, or Stop
// Syncing Offline for an item already chosen. The app keeps .subsonic/offline.txt, the chosen items
// a line each, "<kind>\t<id>\t<name>", written at start and at every change of the selection, which
// the firmware reads to name the row. A press writes /tmp/q2-subsonic.offline,
// "<seq> <add|remove> <kind> <id>", where kind is song, album, playlist or artist, or artistname
// with the artist's name in place of the id (the Library knows its artists by name alone); the app
// answers offlineack=<seq> <words> in its state file, the toast the firmware shows: "Syncing offline
// · 12 songs", "Stopped syncing offline", "Offline via Entire Library" and the rest of toggle's.

// offlineIn is the server's time to answer a request's calls.
var offlineIn = 20 * time.Second

type offlineState struct {
	mu  sync.Mutex
	req string // the .offline request last read, or there at start-up
}

func (d *daemon) offlinePath() string     { return filepath.Join(d.tmp, "q2-"+appID+".offline") }
func (d *daemon) offlineListPath() string { return filepath.Join(d.dir, "offline.txt") }

// offlineInit takes a request left from before this start as answered (the firmware writes one only
// once the app's socket is there) and writes offline.txt from the selection.
func (d *daemon) offlineInit() {
	if b, err := os.ReadFile(d.offlinePath()); err == nil {
		d.off.mu.Lock()
		d.off.req = string(b)
		d.off.mu.Unlock()
	}
	if sel, err := d.selection(); err == nil {
		d.writeOfflineList(sel)
	}
}

// writeOfflineList writes offline.txt for sel; tabs and line breaks in a name become spaces.
func (d *daemon) writeOfflineList(sel selection.Set) {
	var b strings.Builder
	clean := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")
	for _, it := range sel.Items {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", it.Kind, clean.Replace(it.ID), clean.Replace(it.Name))
	}
	if err := library.WriteOwned(d.card, d.offlineListPath(), []byte(b.String())); err != nil {
		d.log.Printf("offline: %v", err)
	}
}

// offlineTick answers the firmware's .offline request, each once; the server is asked in the
// background.
func (d *daemon) offlineTick() {
	b, err := os.ReadFile(d.offlinePath())
	d.off.mu.Lock()
	if err != nil || string(b) == d.off.req {
		d.off.mu.Unlock()
		return
	}
	d.off.req = string(b)
	d.off.mu.Unlock()
	line := strings.TrimRight(strings.SplitN(string(b), "\n", 2)[0], "\r")
	f := strings.SplitN(line, " ", 4)
	seq, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || len(f) < 4 || (f[1] != "add" && f[1] != "remove") || f[3] == "" {
		d.log.Printf("offline: bad request %q", line)
		return
	}
	d.spawn(func() {
		msg := d.offlineSet(f[1] == "add", f[2], f[3])
		d.state.setOfflineAck(fmt.Sprintf("%d %s", seq, msg))
	})
}

// offlineSet chooses the item for Sync (add) or takes it out, and says how that went in words. arg
// is the item's id, or for kind artistname the artist's name, found among the server's artists.
func (d *daemon) offlineSet(add bool, kind, arg string) string {
	cl := d.client()
	if cl == nil {
		return "Sign in to Subsonic first"
	}
	ctx, cancel := context.WithTimeout(context.Background(), offlineIn)
	defer cancel()
	id := arg
	if kind == "artistname" {
		ars, err := cached(d, cl, "artists", func() ([]subsonic.Artist, error) { return cl.Artists(ctx) })
		if err != nil {
			return subsonic.Message(err, d.hostName())
		}
		kind, id = string(selection.KindArtist), ""
		for _, a := range ars {
			if strings.EqualFold(a.Name, arg) {
				id = a.ID
				break
			}
		}
		if id == "" {
			return "Not on the server"
		}
	}
	k := selection.Kind(kind)
	switch k {
	case selection.KindSong, selection.KindAlbum, selection.KindPlaylist, selection.KindArtist:
	default:
		return "Can't sync that offline"
	}
	sel, err := d.selection()
	if err != nil {
		return selDamaged
	}
	if sel.Has(k, id) == add {
		if add {
			return "Already syncing offline"
		}
		return "Not syncing offline"
	}
	return d.toggle(ctx, cl, selection.Key(k, id)).Message
}
