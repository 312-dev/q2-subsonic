package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
)

// Settings > Storage (q2-coda's docs/internals.md#storage), the app's half: with the firmware's
// "storage" word the app keeps storage.tsv in its folder, how much of the card its downloads
// (Subsonic/, the synced songs) and its own folder (.subsonic/: the Stream Cache, covers and the
// app's files) take, its Downloads limit and the items it downloaded, for the firmware's Storage
// page; and it takes the firmware's requests from storage.out: a new limit, or an item to remove
// from Sync. The app measures only what it wrote: the user's own files are the firmware's to count
// (the card's use less these), and nothing outside Subsonic/ and .subsonic/ is read or changed.
// Without the word nothing of this is written.
//
//	#Q2STORE/1	limit=<MB, 0 for none>	downloads=<bytes>	cache=<bytes>
//	i	<pick|auto>	<key>	<bytes>	<op>	<arg>	<name>
//
// Every item today is one the user picked; the app keeps none on its own (auto) yet.

const (
	storageName = "storage.tsv"
	storageOut  = "storage.out"
	stEvery     = 30 * time.Second // storage.tsv is brought up to date this often while the app runs
	stWalkEvery = 10 * time.Minute // .subsonic/ outside the stream cache is walked this often
)

// limitSizes are Downloads' Limit choices in MB, the firmware's; 0 is none.
var limitSizes = []int{0, 4000, 8000, 16000, 32000, 64000}

// stState is what the storage channel keeps between ticks.
type stState struct {
	last    string    // storage.tsv as written last
	at      time.Time // when it was brought up to date last
	own     int64     // .subsonic/ less the stream cache, as walked last
	ownAt   time.Time
	pending bool        // something changed: write at the next tick
	busy    atomic.Bool // a write runs (it may walk the folder: off the tick)
}

// limitBytes is the Downloads limit, 0 for none.
func (s settings) limitBytes() int64 { return int64(max(s.LimitMB, 0)) * 1e6 }

// stTick takes storage.out and brings storage.tsv up to date: at once after a request, else every
// stEvery.
func (d *daemon) stTick() {
	if !d.caps.Storage {
		return
	}
	took := d.stTake()
	d.mu.Lock()
	due := took || d.st.pending || time.Since(d.st.at) >= stEvery
	d.mu.Unlock()
	if due && d.st.busy.CompareAndSwap(false, true) {
		d.spawn(func() { defer d.st.busy.Store(false); d.stWrite() })
	}
}

// stTake applies storage.out's lines, taken by renaming the file so the firmware's next append
// starts a new one; a file a crash left taken is applied first. Whether it took any.
func (d *daemon) stTake() bool {
	out, taking := filepath.Join(d.dir, storageOut), filepath.Join(d.dir, storageOut+".taking")
	if _, err := os.Stat(taking); err != nil {
		if os.Rename(out, taking) != nil {
			return false
		}
	}
	b, err := os.ReadFile(taking)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		op, arg, _ := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		switch op {
		case "limit":
			mb, err := strconv.Atoi(arg)
			if err != nil || !slices.Contains(limitSizes, mb) {
				d.log.Printf("storage: bad limit %q", arg)
				continue
			}
			settingsMu.Lock()
			s := d.settings()
			s.LimitMB = mb
			if err := d.saveSettings(s); err != nil {
				d.log.Printf("storage: limit: %v", err)
			}
			settingsMu.Unlock()
		case "remove":
			d.stRemove(arg)
		}
	}
	os.Remove(taking)
	return true
}

// stRemove takes an item out of Sync, as On This Q2's hold does: its songs go at the next Sync.
func (d *daemon) stRemove(key string) {
	kind, id := parseKey(key)
	sel, err := d.selection()
	if err != nil || !sel.Has(kind, id) {
		return
	}
	sel.Toggle(selection.Item{Kind: kind, ID: id})
	if err := d.saveSelection(sel); err != nil {
		d.log.Printf("storage: remove: %v", err)
	}
}

// stOwn is the size of .subsonic/ less the stream cache, walked at most every stWalkEvery (covers
// are thousands of files); the stream cache's own count is live.
func (d *daemon) stOwn() int64 {
	d.mu.Lock()
	own, at := d.st.own, d.st.ownAt
	d.mu.Unlock()
	if !at.IsZero() && time.Since(at) < stWalkEvery {
		return own
	}
	stream := d.stream.dir()
	own = 0
	filepath.WalkDir(d.dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() && p == stream {
			return filepath.SkipDir
		}
		if fi, ierr := e.Info(); ierr == nil && e.Type().IsRegular() {
			own += fi.Size()
		}
		return nil
	})
	d.mu.Lock()
	d.st.own, d.st.ownAt = own, time.Now()
	d.mu.Unlock()
	return own
}

// stItem is where the firmware opens an item: its page in the app, or On This Q2 for the Entire
// Library and a single song, which have none.
func stItem(kind selection.Kind) (op string, arg bool) {
	switch kind {
	case selection.KindAlbum, selection.KindPlaylist, selection.KindArtist:
		return string(kind), true
	}
	return "onq2", false
}

// storageTSV is storage.tsv's text.
func (d *daemon) storageTSV() string {
	v := d.view()
	var down int64
	for _, e := range v.m.Songs {
		down += e.Size
	}
	cache := d.stOwn() + d.stream.used.Load()
	var b strings.Builder
	fmt.Fprintf(&b, "#Q2STORE/1\tlimit=%d\tdownloads=%d\tcache=%d\n", max(d.settings().LimitMB, 0), down, cache)
	for _, it := range v.sel.Items {
		key := selection.Key(it.Kind, it.ID)
		var n int64
		for _, sid := range v.keyed[key] {
			if e := v.m.Songs[sid]; e != nil {
				n += e.Size
			}
		}
		name := it.Name
		if it.Kind == selection.KindLibrary {
			name = "Entire Library"
		}
		op, hasArg := stItem(it.Kind)
		arg := ""
		if hasArg {
			arg = key
		}
		fmt.Fprintf(&b, "i\tpick\t%s\t%d\t%s\t%s\t%s\n", q2spot.Escape(key), n, op, q2spot.Escape(arg), q2spot.Escape(name))
	}
	return b.String()
}

// stWrite writes storage.tsv when it differs from what was written last.
func (d *daemon) stWrite() {
	text := d.storageTSV()
	d.mu.Lock()
	same := text == d.st.last
	d.st.at, d.st.pending = time.Now(), false
	d.mu.Unlock()
	if same {
		return
	}
	if err := q2.WriteFile(filepath.Join(d.dir, storageName), []byte(text)); err != nil {
		d.log.Printf("storage: %v", err)
		return
	}
	d.mu.Lock()
	d.st.last = text
	d.mu.Unlock()
}

// overLimit is how far a plan would take the downloads past the limit, 0 when within it or with
// none: the downloads now and the plan's new bytes, against limit.
func overLimit(limit, now, need int64) int64 {
	if limit <= 0 || need <= 0 || now+need <= limit {
		return 0
	}
	return now + need - limit
}
