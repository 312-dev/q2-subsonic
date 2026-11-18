package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/catalogue"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

// The Server Library (q2-coda's docs/server-library.md), the app's half: with the firmware's
// "catalogue" word and Settings' Show in Library on, the app keeps catalogue.tsv, the server's
// songs that are not on the card, for the firmware's Library to show beside the card's own; says
// in the state file whether the server answers (server=up or down); and plays what the firmware
// asks for from those rows (.catplay). With the "ratings" word too, stars and likes follow a song
// between the Q2 and the server. Without the words nothing of this is written. With the "libsource" word
// the firmware's Library setting Streaming Songs (Local + Server) must be on too: while it is off
// (q2-addon.library says "local") the app pauses, asking the server nothing and keeping its files,
// and it goes on where it was when the switch is turned on again.

const (
	appID    = "subsonic"  // the app's folder is .<appID>, its /tmp files q2-<appID>.*
	slEvery  = time.Minute // the server is pinged this often while the Server Library runs (slActive)
	slPingIn = 10 * time.Second
)

// batteryStatus is where the Q2's charger says whether it charges (the AXP2101's battery supply).
var batteryStatus = "/sys/class/power_supply/battery/status"

// serverLib is the Server Library's state while the app runs.
type serverLib struct {
	on        atomic.Bool // Show in Library, as last saved
	busy      atomic.Bool // a connection's checks, a walk or a catalogue write runs: one at a time
	checking  atomic.Bool // a connection's checks or a walk asks the server now: the state file's sync=check
	streaming atomic.Bool // Streaming Songs, as q2-addon.library last said; always on without "libsource"

	mu       sync.Mutex
	up       int8      // the server answered (1), didn't (-1), not asked yet (0)
	nextPing time.Time // when the server is asked next
	due      bool      // the server's hint changed: a walk waits for the Q2 to be idle or charging
	resync   bool      // Resync Library was pressed: the next check walks whatever the hint says
	rewrite  bool      // Streaming Songs is on again: the next check writes the catalogue first
	note     string    // how far a walk has come, for Settings
	errMsg   string    // the last failure that was not a lost server (a refused sign-in, say)
	songs    int       // songs of the last walk
	walked   time.Time // when it ended
	catplay  string    // the .catplay last answered (or there at start-up)
	settled  string    // coversKey when a round of covers last found nothing it could fetch
	planned  int       // rounds of covers that read the store and the folders (for tests)
}

func (d *daemon) slInit() {
	d.sl.on.Store(d.caps.Catalogue && d.settings().Library)
	d.sl.streaming.Store(d.slSource())
	if b, err := os.ReadFile(d.catplayPath()); err == nil {
		d.sl.catplay = string(b)
	}
}

func (d *daemon) catplayPath() string { return filepath.Join(d.tmp, "q2-"+appID+".catplay") }

// sourcePath is where firmware with the "libsource" word says whether Streaming Songs is on:
// "server" or "local", written at boot and at every change of the switch.
func (d *daemon) sourcePath() string { return filepath.Join(d.tmp, "q2-addon.library") }

// slSource is whether Streaming Songs lets the Server Library run: on firmware without the
// "libsource" word always; on one with it, only while q2-addon.library says "server" (a missing
// file is the switch off).
func (d *daemon) slSource() bool {
	if !d.caps.Libsource {
		return true
	}
	b, _ := os.ReadFile(d.sourcePath())
	return strings.TrimSpace(string(b)) == "server"
}

// slFollowSource reads the Streaming Songs switch again. Turned off, the Server Library pauses
// (slActive is false: no ping, walk or cover, and the files stay); turned on, the next check comes
// at once: it writes the catalogue again, for the syncs and the stream cache that changed the card
// while it was paused, then asks the server, and walks only if its library changed meanwhile.
func (d *daemon) slFollowSource() {
	if !d.caps.Catalogue || !d.caps.Libsource {
		return
	}
	on := d.slSource()
	if d.sl.streaming.Swap(on) == on {
		return
	}
	if !on {
		d.log.Printf("server library: paused, Streaming Songs is off")
		return
	}
	d.log.Printf("server library: Streaming Songs is on again")
	d.sl.mu.Lock()
	d.sl.nextPing, d.sl.up, d.sl.rewrite = time.Time{}, 0, true
	d.sl.mu.Unlock()
}

func (d *daemon) store() catalogue.Store {
	return catalogue.Store{Card: d.card, Path: filepath.Join(d.dir, catalogue.SongsName)}
}

func (d *daemon) playlistsFile() catalogue.Playlists {
	return catalogue.Playlists{Card: d.card, Path: filepath.Join(d.dir, catalogue.PlaylistsName)}
}

// slActive is whether the Server Library runs: the firmware's word, Show in Library, signed in,
// and on firmware with the "libsource" word, Streaming Songs on.
func (d *daemon) slActive() bool {
	return d.caps.Catalogue && d.sl.on.Load() && d.sl.streaming.Load() && d.acct() != ""
}

// slTick is the Server Library's part of the daemon's tick: the Streaming Songs switch and the
// firmware's play requests at once, the server every slEvery.
func (d *daemon) slTick() {
	d.slFollowSource()
	if !d.slActive() {
		return
	}
	d.slCatplay()
	d.sl.mu.Lock()
	if time.Now().Before(d.sl.nextPing) || d.sl.busy.Load() {
		d.sl.mu.Unlock()
		return
	}
	d.sl.nextPing = time.Now().Add(slEvery)
	d.sl.mu.Unlock()
	if d.sl.busy.CompareAndSwap(false, true) {
		d.spawn(func() { defer d.sl.busy.Store(false); d.slCheck() })
	}
}

// slCheck pings the server. When it answers after not answering (or for the first time since the
// app started), the connection's checks run (slConnect); else a walk the hint asked for runs once
// the Q2 is idle or charging. After a pause it writes the catalogue first (slFollowSource).
func (d *daemon) slCheck() {
	d.sl.mu.Lock()
	rewrite := d.sl.rewrite
	d.sl.rewrite = false
	d.sl.mu.Unlock()
	if rewrite {
		d.slWrite()
	}
	cl := d.client()
	if cl == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), slPingIn)
	err := cl.Ping(ctx)
	cancel()
	was := d.slServer(err)
	switch {
	case err != nil:
		return
	case was != 1, d.caps.Playlists && catalogue.PlsPending(d.dir):
		d.sl.checking.Store(true)
		defer d.sl.checking.Store(false)
		d.slConnect(cl) // a connection's checks, or a playlist the Q2 changed meanwhile
		return
	}
	d.sl.mu.Lock()
	walk := d.sl.resync || d.sl.due && (d.slIdle() || charging())
	d.sl.mu.Unlock()
	if walk {
		d.sl.checking.Store(true)
		defer d.sl.checking.Store(false)
		d.slWalk(cl)
		return
	}
	d.slCovers(cl)
}

const (
	slCoverPass = 30 * time.Second // a round of covers at most: the next check, a rewrite, wait no longer
	slCoverConc = 2                // covers asked for at once
	slCoverIn   = 10 * time.Second // one cover's request
)

func (d *daemon) covers() catalogue.Covers {
	return catalogue.Covers{Card: d.card, Dir: filepath.Join(d.dir, catalogue.CoversName)}
}

// bigCovers is the car's set, covers/512 (catalogue.BigName).
func (d *daemon) bigCovers() catalogue.Covers {
	return catalogue.Covers{Card: d.card, Dir: filepath.Join(d.dir, catalogue.CoversName, catalogue.BigName),
		Px: catalogue.BigPx, Body: catalogue.BigBody}
}

// slCovers is a round of the catalogue's album covers (catalogue.Covers): while the app's network
// is quiet (slQuiet) or the Q2 charges, and the server answers, the albums the folder lacks are
// asked for, slCoverConc at a time and for slCoverPass at most, through the network slot a sync or
// a stream takes first. What was saved stays, so the next round, a minute on, goes on from there.
// A cover the server says it hasn't is noted and not asked again; a lost server ends the round,
// and so does a sync starting or a song of the stream to fetch. Once the 160 px set has nothing
// left to fetch, the rounds fetch the car's 512 px set (bigCovers) the same way. Once neither has
// anything it can fetch (all there, noted, or no room), the rounds do nothing until coversKey
// changes.
func (d *daemon) slCovers(cl *subsonic.Client) {
	if !d.slQuiet() && !charging() {
		return
	}
	d.sl.mu.Lock()
	settled := d.sl.settled != "" && d.sl.settled == d.coversKey()
	if !settled {
		d.sl.planned++
	}
	d.sl.mu.Unlock()
	if settled {
		return
	}
	b := catalogue.Build{Card: d.card, App: appID, Account: d.acct(), Synced: d.syncedPaths(), Local: d.stream.copies()}
	albums, err := catalogue.Albums(d.store(), b)
	if err != nil {
		d.slFail("covers", err)
		return
	}
	pls, _ := d.playlistsFile().Load(d.acct()) // the playlists' covers after the albums'
	albums = append(albums, catalogue.PlaylistCovers(pls)...)
	art := map[string]string{} // a playlist's file name: the coverArt id the server knows it by
	for _, a := range albums {
		if a.Art != "" {
			art[a.ID] = a.Art
		}
	}
	cv := d.covers()
	pass, err := cv.Plan(albums)
	if err == nil && len(pass.Todo) == 0 { // the Q2's own all there: the car's larger ones
		cv = d.bigCovers()
		pass, err = cv.Plan(albums)
	}
	if err != nil || len(pass.Todo) == 0 {
		if err != nil {
			d.slFail("covers", err)
		} else {
			d.slSettle()
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), slCoverPass)
	defer cancel()
	start := time.Now()
	var mu sync.Mutex
	var saved, none int
	var lost error
	ids := make(chan string)
	var wg sync.WaitGroup
	for range slCoverConc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ids {
				ask := id
				if a := art[id]; a != "" {
					ask = a
				}
				jpg, err := d.fetchCover(ctx, cl, ask, cv, false)
				mu.Lock()
				var ae *subsonic.APIError
				switch {
				case err == nil:
					if err = pass.Save(id, jpg); err == nil {
						saved++
					} else if lost == nil {
						lost = err // the card is full, or its folder can't be written
					}
				case err == errNoCover || errors.As(err, &ae) && (ae.HTTP == 404 || ae.Code == 70):
					// the server answered that it has none, or sent what the Q2 can't show
					pass.None(id)
					none++
				case ctx.Err() == nil && lost == nil:
					lost = err
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range pass.Todo {
		mu.Lock()
		stop := lost != nil
		mu.Unlock()
		if stop || ctx.Err() != nil || !d.slQuiet() && !charging() || !d.slActive() {
			break
		}
		select {
		case ids <- id:
		case <-ctx.Done():
		}
	}
	close(ids)
	wg.Wait()
	d.log.Printf("server library: covers (%d px): %d saved, %d without, %d left, %d KiB in %v", cv.Side(), saved, none,
		len(pass.Todo)-saved-none, pass.Used()>>10, time.Since(start).Round(time.Millisecond))
	if errors.Is(lost, catalogue.ErrCardFull) {
		d.slSettle() // no room: asked again once the card or the albums change
	}
	if lost != nil && subsonic.ConnectionLost(lost) {
		d.slServer(lost)
	} else if lost != nil && !errors.Is(lost, catalogue.ErrCardFull) {
		d.log.Printf("server library: covers: %s", applog.Redact(lost.Error()))
	}
}

// coversKey is what a round of covers depends on, read without opening a file: the account, the
// store, the playlists and the manifest (the synced songs, whose albums are the card's) by size and
// time, both folders' times (a cover written or deleted, the folder gone with Show in Library or
// Sign Out), and whether the card is above CoverFloor. A round that found nothing it could fetch
// (slSettle) is not run again until it changes: reading the store and every cover's size took
// seconds of the Q2's CPU a minute. The stream cache's songs are left out: they keep the cover of
// a song gone from the server, whose album the server has no cover for anyway.
func (d *daemon) coversKey() string {
	var b strings.Builder
	b.WriteString(d.acct())
	for _, p := range []string{d.store().Path, d.playlistsFile().Path, filepath.Join(d.card, ".subsonic", "manifest.json"),
		d.covers().Dir, d.bigCovers().Dir} {
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "|%d %d", st.Size(), st.ModTime().UnixNano())
		} else {
			b.WriteString("|-")
		}
	}
	fmt.Fprintf(&b, "|%t", d.free(d.card) >= catalogue.CoverFloor+catalogue.BigBody)
	return b.String()
}

// slSettle notes that the covers' rounds have nothing to do until coversKey changes.
func (d *daemon) slSettle() {
	key := d.coversKey()
	d.sl.mu.Lock()
	d.sl.settled = key
	d.sl.mu.Unlock()
}

// fetchCover is album id's cover from the server, cv's side at most, as a baseline JPEG. With prio
// (a cover the car is waiting for) it takes the network slot before a sync's next file, as a
// stream's song does.
func (d *daemon) fetchCover(ctx context.Context, cl *subsonic.Client, id string, cv catalogue.Covers, prio bool) ([]byte, error) {
	if err := syncer.Net.Acquire(ctx, prio); err != nil {
		return nil, err
	}
	defer syncer.Net.Release()
	quiet := *cl
	quiet.Log = nil // a line per cover would push the walk's history out of log.txt
	ctx, cancel := context.WithTimeout(ctx, slCoverIn)
	defer cancel()
	resp, err := quiet.Get(ctx, quiet.CoverURLSize(id, cv.Side()), 0)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(cv.MaxBody())+1))
	if err != nil {
		return nil, err
	}
	if len(b) > cv.MaxBody() {
		return nil, errNoCover
	}
	if cv.Side() > catalogue.CoverPx { // the car's: kept as the server sent it when it is one already
		if jpg, ok := asBaseline(b, cv.Side()); ok {
			return jpg, nil
		}
	}
	return baselineMax(b, cv.Side())
}

// slServer records what an answer (or none) says of the server and writes it to the state file:
// only a lost connection is "down"; an error the server sent, a refused sign-in included, is
// shown in Settings, never taken as offline. It returns what was known before.
func (d *daemon) slServer(err error) (was int8) {
	up, msg := int8(1), ""
	switch {
	case subsonic.ConnectionLost(err):
		up = -1
	case err != nil:
		msg = subsonic.Message(err, d.hostName())
	}
	d.sl.mu.Lock()
	was, d.sl.up = d.sl.up, up
	if err != nil || was != 1 {
		d.sl.errMsg = msg
	}
	d.sl.mu.Unlock()
	if d.slActive() {
		d.state.setServer(map[int8]string{1: "up", -1: "down"}[up])
	}
	return was
}

// slPinged is the streamer's ping after a song failed: the state file follows it at once.
func (d *daemon) slPinged(err error) {
	if d.slActive() {
		d.slServer(err)
	}
}

// slIdle is whether the Q2 is idle for a walk: no sync runs and no list streams. A walk ends in a
// new catalogue, which the firmware imports, so it waits until nothing plays.
func (d *daemon) slIdle() bool { return !d.runner.Busy() && !d.stream.active() }

// slQuiet is whether a cover may be fetched: no sync runs and the stream has no song to fetch
// (streamer.busy). A list playing what it already fetched, or a song played from the cache, is
// quiet: covers are a few KB each and wait for the network slot syncs and streams take first.
func (d *daemon) slQuiet() bool { return !d.runner.Busy() && !d.stream.busy() }

func charging() bool {
	b, _ := os.ReadFile(batteryStatus)
	s := strings.TrimSpace(string(b))
	return s == "Charging" || s == "Full"
}

// slFail keeps a failure for Settings and the log. A lost server is marked down instead.
func (d *daemon) slFail(what string, err error) {
	d.log.Printf("server library: %s: %v", what, err)
	if subsonic.ConnectionLost(err) {
		d.slServer(err)
		return
	}
	d.sl.mu.Lock()
	d.sl.errMsg = subsonic.Message(err, d.hostName())
	d.sl.mu.Unlock()
}

// slConnect is what runs each time the server answers again: the Q2's stars and likes are sent first
// (a rating has no date on the server, so the change waiting on the Q2 is the newer), then the
// server's hint is read: no walk yet is the first copy, made now; a changed hint is a walk when
// the Q2 is idle or charging. Playlists (by their changed dates) and stars are checked every time,
// as a scan doesn't cover them.
func (d *daemon) slConnect(cl *subsonic.Client) {
	ctx := context.Background()
	cl = cl.Within(checkCallMax)
	acct := d.acct()
	st := d.store()
	sent := d.slSendRatings(ctx, cl)
	d.slSendPlaylists(ctx, cl) // before the playlists are read back, which then have the Q2's changes
	hint, err := d.slHint(ctx, cl, st, acct)
	if err != nil {
		d.slFail("hint", err)
		return
	}
	head, have := st.Head(acct)
	d.sl.mu.Lock()
	if have && head.Hint != hint {
		d.sl.due = true
	}
	walkNow := !have || d.sl.resync || d.sl.due && (d.slIdle() || charging())
	d.sl.mu.Unlock()
	write := false
	old, _ := d.playlistsFile().Load(acct)
	pls, changed, err := catalogue.Refresh(ctx, cl, old)
	if err != nil {
		d.slFail("playlists", err)
		return
	}
	if changed || !catalogue.SameDates(old, pls) {
		if err := d.playlistsFile().Save(acct, pls); err != nil {
			d.slFail("playlists", err)
			return
		}
		write = changed
	}
	if walkNow {
		if !d.slWalkWith(ctx, cl, hint, sent) {
			return
		}
		write = false // the walk wrote it
	} else if have {
		if starred, err := cl.StarredSongs(ctx); err != nil {
			d.slFail("starred", err)
		} else if ch, err := st.StarChanges(acct, starred, sent); err != nil {
			d.slFail("starred", err)
		} else {
			d.slRatingsIn(ch)
		}
	}
	if write || have && !d.slCatalogueIsMine(acct) {
		d.slWrite()
	}
}

// slHint is the server's word on whether its library changed: getScanStatus's lastScan and count
// (Navidrome's), else getIndexes' lastModified. A server that is scanning gives the hint the store
// has, so the walk waits for the scan's end.
func (d *daemon) slHint(ctx context.Context, cl *subsonic.Client, st catalogue.Store, acct string) (catalogue.Hint, error) {
	head, _ := st.Head(acct)
	lastScan, count, scanning, err := cl.ScanStatus(ctx)
	switch {
	case err == nil && scanning:
		return head.Hint, nil
	case err == nil && lastScan != "":
		return catalogue.Hint{LastScan: lastScan, Count: count}, nil
	case err != nil && !subsonic.Unsupported(err) && !errors.Is(err, subsonic.ErrNotFound):
		var ae *subsonic.APIError
		if !errors.As(err, &ae) || ae.Code == 0 || ae.Code == 40 || ae.Code == 44 {
			return catalogue.Hint{}, err // lost, or refused: no hint at all
		}
	}
	ms, err := cl.IndexesModified(ctx, head.Hint.Modified)
	if err != nil {
		return catalogue.Hint{}, err
	}
	if ms == 0 { // unchanged since, on a server that leaves the date out then
		ms = head.Hint.Modified
	}
	return catalogue.Hint{Modified: ms, Count: count}, nil
}

// slWalk walks the library now, with the hint as it is now.
func (d *daemon) slWalk(cl *subsonic.Client) {
	ctx := context.Background()
	cl = cl.Within(checkCallMax)
	hint, err := d.slHint(ctx, cl, d.store(), d.acct())
	if err != nil {
		d.slFail("hint", err)
		return
	}
	d.slWalkWith(ctx, cl, hint, nil)
}

// slWalkWith is a full walk: every song paged to the store, the stars and likes that differ to
// ratings.in, and the catalogue written. It reports whether it went through.
func (d *daemon) slWalkWith(ctx context.Context, cl *subsonic.Client, hint catalogue.Hint, sent map[string]catalogue.Rated) bool {
	acct := d.acct()
	start := time.Now()
	res, err := d.store().Walk(ctx, cl, acct, hint, sent, func(n int) {
		d.sl.mu.Lock()
		d.sl.note = "Checking · " + plural(n, "song")
		d.sl.mu.Unlock()
	})
	d.sl.mu.Lock()
	d.sl.note = ""
	if err == nil {
		d.sl.due, d.sl.resync, d.sl.songs, d.sl.walked, d.sl.errMsg = false, false, res.Songs, time.Now(), ""
	}
	d.sl.mu.Unlock()
	if err != nil {
		d.slFail("walk", err)
		return false
	}
	d.log.Printf("server library: walk: %d songs (%d gone) in %d requests, %v", res.Songs, res.Gone, res.Requests,
		time.Since(start).Round(time.Millisecond))
	d.slRatingsIn(res.Changes)
	d.slWrite()
	return true
}

// slCatalogueIsMine is whether catalogue.tsv is there and the signed-in account's.
func (d *daemon) slCatalogueIsMine(acct string) bool {
	f, err := os.Open(filepath.Join(d.dir, catalogue.CatName))
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 256)
	n, _ := f.Read(b)
	return strings.Contains(string(b[:n]), "\taccount="+acct+"\t")
}

// slWrite writes catalogue.tsv from the store, the playlists, the manifest and the stream cache.
func (d *daemon) slWrite() {
	acct := d.acct()
	if acct == "" || !d.slActive() {
		return
	}
	pls, _ := d.playlistsFile().Load(acct)
	b := catalogue.Build{Card: d.card, App: appID, Account: acct, Synced: d.syncedPaths(), Local: d.stream.copies()}
	start := time.Now()
	seq, rows, err := catalogue.Write(filepath.Join(d.dir, catalogue.CatName), d.store(), pls, b)
	if err != nil {
		d.slFail("catalogue", err)
		return
	}
	d.log.Printf("server library: catalogue %d: %d rows in %v", seq, rows, time.Since(start).Round(time.Millisecond))
}

// slRewrite writes the catalogue again in the background, as after a sync, which changed what is
// on the card; no request to the server.
func (d *daemon) slRewrite() {
	if !d.slActive() {
		return
	}
	d.spawn(func() {
		for !d.sl.busy.CompareAndSwap(false, true) { // after the check or walk running now
			time.Sleep(time.Second)
		}
		defer d.sl.busy.Store(false)
		d.slWrite()
	})
}

// syncedPaths is the songs the app synced, by id: their card paths.
func (d *daemon) syncedPaths() map[string]string {
	out := map[string]string{}
	for id, e := range d.manifest().Songs {
		out[id] = e.Path
	}
	return out
}

// copies is the stream cache's whole songs of the signed-in account, by id, of any quality: a
// song kept at another quality than the one streamed now still plays offline.
func (s *streamer) copies() map[string]string {
	out := map[string]string{}
	a := ""
	if s.acct != nil {
		a = s.acct()
	}
	if a == "" {
		return out
	}
	for _, q := range subdirs(filepath.Join(s.dir(), a)) {
		for _, x := range subdirs(q) {
			ents, _ := os.ReadDir(x)
			for _, e := range ents {
				if id := streamID(e.Name()); id != "" && e.Type().IsRegular() && library.IsSong(e.Name()) { // not a .part or .lrc
					out[id] = filepath.Join(x, e.Name())
				}
			}
		}
	}
	return out
}

// slAvail is the streamer's avail hook: catalogue.avail follows the stream cache.
func (d *daemon) slAvail(path string, kept bool) {
	if !d.slActive() || !d.stream.inCache(path) {
		return
	}
	id := streamID(path)
	if id == "" {
		return
	}
	if !kept {
		path = ""
	}
	if err := catalogue.Avail(d.card, d.dir, id, path); err != nil {
		d.log.Printf("server library: avail: %v", err)
	}
}

// listenKey is the firmware's listen_key of a song: its card path when synced, else its server
// row's fileurl.
func (d *daemon) listenKey(synced map[string]string) func(catalogue.Change) string {
	return func(c catalogue.Change) string {
		if p := synced[c.SID]; p != "" {
			return catalogue.ListenKey(p)
		}
		return catalogue.ListenKey(catalogue.FileURL(d.card, appID, c.Song.AlbumID, c.SID, c.Song.Suffix))
	}
}

// slRatingsIn writes the server's stars and likes that changed to ratings.in, with the firmware's "ratings".
func (d *daemon) slRatingsIn(ch []catalogue.Change) {
	if !d.caps.Ratings || len(ch) == 0 {
		return
	}
	if err := catalogue.WriteIn(d.card, d.dir, ch, d.listenKey(d.syncedPaths())); err != nil {
		d.log.Printf("server library: ratings.in: %v", err)
		return
	}
	d.log.Printf("server library: %d ratings from the server", len(ch))
}

// slSendRatings sends ratings.out with the firmware's "ratings": the stars and likes given on the
// Q2 go to the server, and what didn't go waits for the next connection. A server whose star is
// newer than the Q2's like wins, and its like goes back in ratings.in. It returns the stars and
// likes the server now has from the Q2, by song id.
func (d *daemon) slSendRatings(ctx context.Context, cl *subsonic.Client) map[string]catalogue.Rated {
	if !d.caps.Ratings {
		return nil
	}
	outs, err := catalogue.TakeOut(d.card, d.dir)
	if err != nil || len(outs) == 0 {
		if err != nil {
			d.log.Printf("server library: ratings.out: %v", err)
		}
		return nil
	}
	synced := d.syncedPaths()
	var byKey map[string]string
	sidOf := func(key string) string {
		if byKey == nil { // only for a line without a sid: synced files, then the server rows
			byKey = map[string]string{}
			for id, p := range synced {
				byKey[catalogue.ListenKey(p)] = id
			}
			d.store().Each(d.acct(), func(s catalogue.Song) error {
				byKey[catalogue.ListenKey(catalogue.FileURL(d.card, appID, s.AlbumID, s.ID, s.Suffix))] = s.ID
				return nil
			})
		}
		return byKey[key]
	}
	res, err := catalogue.Send(ctx, cl, outs, sidOf)
	if kerr := catalogue.KeepOut(d.card, d.dir, res.Rest); kerr != nil {
		d.log.Printf("server library: ratings.out: %v", kerr)
	}
	d.log.Printf("server library: %d ratings sent, %d kept by the server's newer star, %d left, %d dropped",
		len(res.Values), len(res.Wins), len(res.Rest), res.Dropped)
	if err != nil {
		d.slFail("ratings", err)
	}
	d.slRatingsIn(res.Wins)
	if len(res.Values) > 0 { // the store follows, so a walk or star check doesn't send them back
		d.store().Rewrite(d.acct(), func(s catalogue.Song) (catalogue.Song, bool) {
			if v, ok := res.Values[s.ID]; ok {
				s.Rating = v.Stars
				if v.Like && s.Starred == "" {
					s.Starred = time.Now().UTC().Format(time.RFC3339)
				} else if !v.Like {
					s.Starred = ""
				}
			}
			return s, true
		})
	}
	return res.Values
}

// slSendPlaylists sends playlists.out with the firmware's "playlists": each server playlist the Q2
// changed gets its songs, its name or its deletion on the server, and what didn't go waits for the
// next connection.
func (d *daemon) slSendPlaylists(ctx context.Context, cl *subsonic.Client) {
	if !d.caps.Playlists {
		return
	}
	outs, err := catalogue.TakePls(d.card, d.dir)
	if err != nil || len(outs) == 0 {
		if err != nil {
			d.log.Printf("server library: playlists.out: %v", err)
		}
		return
	}
	sent, rest, err := catalogue.SendPls(ctx, cl, outs)
	if kerr := catalogue.KeepPls(d.card, d.dir, rest); kerr != nil {
		d.log.Printf("server library: playlists.out: %v", kerr)
	}
	d.log.Printf("server library: %d playlist changes sent, %d left", sent, len(rest))
	if err != nil {
		d.slFail("playlists out", err)
	}
}

// slCatplay answers the firmware's .catplay, a play pressed on a Library list with server rows:
// "<seq> <start>", then a path a line, the list's rows in order. A server row's path is its
// fileurl (catalogue.FileURL), any other a card file that plays as it is. The answer is the usual
// .play, with the request's seq.
func (d *daemon) slCatplay() {
	if !d.caps.Playlist {
		return
	}
	b, err := os.ReadFile(d.catplayPath())
	if err != nil || string(b) == d.sl.catplay {
		return
	}
	d.sl.catplay = string(b)
	ls := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for i := range ls {
		ls[i] = strings.TrimRight(ls[i], "\r")
	}
	f := strings.Fields(ls[0])
	if len(f) != 2 {
		return
	}
	seq, err1 := strconv.ParseInt(f[0], 10, 64)
	from, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || from < 0 || from >= len(ls)-1 {
		d.log.Printf("server library: bad .catplay %q", ls[0])
		return
	}
	paths := ls[1:]
	want := map[string]bool{}
	for _, p := range paths {
		if id := catalogue.SIDOf(d.card, appID, p); id != "" {
			want[id] = true
		}
	}
	found := d.store().Find(d.acct(), want)
	var songs []subsonic.Song
	var local []string
	start := 0
	for i, p := range paths {
		if i == from {
			start = len(songs)
		}
		if id := catalogue.SIDOf(d.card, appID, p); id != "" {
			if s, ok := found[id]; ok {
				songs, local = append(songs, s.Subsonic()), append(local, "")
			}
			continue
		}
		if filepath.Clean(p) == p && strings.HasPrefix(p, d.card+"/") { // a card file, without a ".." segment
			songs = append(songs, subsonic.Song{Title: strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))})
			local = append(local, p)
		}
	}
	if len(songs) == 0 {
		return
	}
	start = min(start, len(songs)-1)
	n := d.nextPlay()
	d.spawn(func() {
		d.playMu.Lock()
		defer d.playMu.Unlock()
		if d.latestPlay(n) {
			d.stream.startCat(songs, local, start, &catPlay{seq: seq, local: d.stream.copies()})
		}
	})
}

// slSettings is Settings' rows for the Server Library: Show in Library, and while it is on,
// Resync Library with how the last walk went. The row says On only while the Server Library runs
// (slActive): Show in Library saved on while Streaming Songs is off is Off, as the Library shows no
// server songs then. Off on firmware with the "libsource" word, the row is a "needslib" row: the
// firmware turns Streaming Songs on (asking first) before it sends the press.
func (d *daemon) slSettings() []q2spot.Row {
	if !d.caps.Catalogue {
		return nil
	}
	if !d.slActive() {
		row := q2spot.Row{Kind: "action", URI: "showlib", Title: "Show in Library: Off",
			Subtitle: "Streaming songs beside the card's", Context: "on"}
		if d.caps.Libsource {
			row.Kind = "needslib"
		}
		if d.sl.on.Load() && !d.sl.streaming.Load() {
			row.Subtitle = "Streaming Songs is off"
		}
		return []q2spot.Row{row}
	}
	d.sl.mu.Lock()
	sub := d.sl.note
	switch {
	case sub != "":
	case d.sl.errMsg != "":
		sub = d.sl.errMsg
	case d.sl.up == -1:
		sub = "Server not answering"
	case !d.sl.walked.IsZero():
		sub = plural(d.sl.songs, "song") + " · checked " + when(d.sl.walked, time.Now())
	case d.sl.up == 0 || d.sl.busy.Load():
		sub = "Checking…"
	}
	d.sl.mu.Unlock()
	return []q2spot.Row{act("showlib", "Show in Library: On", "off"),
		{Kind: "action", URI: "resync", Title: "Resync Library", Subtitle: sub}}
}

// slToggle is Show in Library's press. The row names what it turns the setting to, "on" or "off"
// (slSettings), as the row's On or Off can be the switch's and not the setting's: Show in Library
// saved on while Streaming Songs is off shows Off, and its "needslib" press comes once the
// firmware has turned the switch on, so the switch is read first. A press without either turns the
// saved setting over. On, the server is asked at once; off, the files the firmware reads go (the
// app's own records stay, so turning it on again needs no first copy).
func (d *daemon) slToggle(s settings, to string) q2spot.Reply {
	d.slFollowSource()
	switch to {
	case "on":
		s.Library = true
	case "off":
		s.Library = false
	default:
		s.Library = !s.Library
	}
	if err := d.saveSettings(s); err != nil {
		return d.settingErr(err)
	}
	d.sl.on.Store(s.Library)
	d.sl.mu.Lock()
	d.sl.nextPing, d.sl.up = time.Time{}, 0
	d.sl.mu.Unlock()
	if !s.Library {
		catalogue.Remove(d.card, d.dir)
		catalogue.RemoveCovers(d.card, d.dir)
		d.state.setServer("")
	}
	if s.Library && !d.sl.streaming.Load() { // saved on, but the Library shows no server songs yet
		return q2spot.Reply{Message: "Needs Streaming Songs"}
	}
	return q2spot.Reply{Message: s.libraryName()}
}

// slResync is Resync Library's press: a walk at the next check, now.
func (d *daemon) slResync() q2spot.Reply {
	if !d.slActive() {
		return q2spot.Fail(q2spot.Unavailable, "Show in Library is off")
	}
	d.sl.mu.Lock()
	d.sl.resync, d.sl.nextPing = true, time.Time{}
	d.sl.mu.Unlock()
	return q2spot.Reply{Message: "Resyncing the library"}
}

// slSignOut forgets the account's Server Library: the firmware's files and the app's records of
// the server go, as its songs can't play for another account.
func (d *daemon) slSignOut() {
	catalogue.Remove(d.card, d.dir)
	catalogue.RemoveCovers(d.card, d.dir)
	for _, n := range []string{catalogue.SongsName, catalogue.PlaylistsName, catalogue.OutName + ".sending"} {
		if err := library.Remove(d.card, filepath.Join(d.dir, n)); err != nil {
			d.log.Printf("sign out: %s: %v", n, err)
		}
	}
	d.state.setServer("")
	d.sl.mu.Lock()
	d.sl.up, d.sl.due, d.sl.resync, d.sl.songs, d.sl.walked, d.sl.errMsg = 0, false, false, 0, time.Time{}, ""
	d.sl.mu.Unlock()
}

func (s settings) libraryName() string {
	return fmt.Sprintf("Show in Library: %s", map[bool]string{true: "On", false: "Off"}[s.Library])
}
