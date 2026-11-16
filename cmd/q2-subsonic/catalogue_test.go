package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/catalogue"
	"github.com/andsuor/q2-subsonic/internal/creds"
	"github.com/andsuor/q2-subsonic/internal/fakesonic"
	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

type catRig struct {
	*daemon
	f   *fakesonic.Server
	tmp string
}

// newCatRig is a daemon signed in to a fakesonic server with the firmware's words caps and Show
// in Library on.
func newCatRig(t *testing.T, caps string, songs []fakesonic.Song) *catRig {
	t.Helper()
	card, tmp := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(tmp, "q2-addon.caps"), []byte(caps+"\n"), 0o644)
	f := fakesonic.New()
	f.Songs, f.Count, f.LastScan = songs, int64(len(songs)), "2026-11-14T01:00:00Z"
	d := newDaemon(filepath.Join(card, ".subsonic"), card, tmp)
	d.newClient = func(c subsonic.Creds) (*subsonic.Client, error) { return subsonic.New(c, f.Client()) }
	d.runner.CardMounted = func() bool { return true }
	d.free = func(string) int64 { return 1 << 40 }
	if err := creds.Save(d.dir, subsonic.Creds{Server: f.URL, User: "u", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	d.saveSettings(settings{Library: true})
	d.slInit()
	t.Cleanup(func() {
		d.stream.stop()
		d.runner.Cancel()
		f.CloseClientConnections()
		f.Close()
		d.bg.Wait()
	})
	return &catRig{d, f, tmp}
}

// check runs one Server Library check now and waits for it.
func (r *catRig) check(t *testing.T) {
	t.Helper()
	r.sl.mu.Lock()
	r.sl.nextPing = time.Time{}
	r.sl.mu.Unlock()
	r.slTick()
	r.bg.Wait()
}

func (r *catRig) file(name string) string { return filepath.Join(r.dir, name) }

func (r *catRig) state() string {
	b, _ := os.ReadFile(filepath.Join(r.tmp, "q2-subsonic.state"))
	return string(b)
}

func (r *catRig) calls(op string) int {
	r.f.Mu.Lock()
	defer r.f.Mu.Unlock()
	return r.f.Calls[op]
}

func catLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// Firmware without the word gets nothing new: no catalogue, no server key, no request, no row.
func TestServerLibraryNeedsTheWord(t *testing.T) {
	r := newCatRig(t, "playlist sections", fakesonic.Library(3))
	r.check(t)
	if _, err := os.Stat(r.file(catalogue.CatName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("catalogue written")
	}
	if strings.Contains(r.state(), "server=") || len(r.f.Calls) != 0 {
		t.Fatalf("state %q, calls %v", r.state(), r.f.Calls)
	}
	if _, ok := find(r.settingsOp("settings"), "Show in Library"); ok {
		t.Fatal("Show in Library offered")
	}
}

// The first connection is the first copy: every song paged, the playlists, the catalogue written,
// the server's hearts to ratings.in, and server=up.
func TestFirstCopy(t *testing.T) {
	lib := fakesonic.Library(1203)
	lib[0].Rating = 4
	r := newCatRig(t, "playlist catalogue ratings", lib)
	r.f.Playlists = []fakesonic.Playlist{{ID: "p1", Name: "Mix", Changed: "t1", SIDs: []string{"s1", "s2"}}}
	r.check(t)
	l := catLines(t, r.file(catalogue.CatName))
	if want := fmt.Sprintf("#Q2CAT/1\tseq=1\taccount=%s\trows=1206", r.acct()); l[0] != want || len(l) != 1207 {
		t.Fatalf("%q, %d lines", l[0], len(l))
	}
	if r.calls("search3") != 3 || !strings.Contains(r.state(), "server=up\n") {
		t.Fatalf("calls %v, state %q", r.f.Calls, r.state())
	}
	in := catLines(t, r.file(catalogue.InName))
	key := catalogue.ListenKey(catalogue.FileURL(r.card, "subsonic", "al0", "s1", "flac"))
	if len(in) != 1 || !strings.HasPrefix(in[0], key+"\ts1\t4\t") {
		t.Fatal(in)
	}
	if sub := r.settingsOp("settings"); !slices.Contains(titles(sub), "Resync Library") {
		t.Fatal(titles(sub))
	}
}

// Later connections ask for the hint, the playlists and the stars, and walk only when the hint
// moved and the Q2 is idle or charging; a song gone from the server is marked, not removed.
func TestLaterConnections(t *testing.T) {
	r := newCatRig(t, "playlist catalogue ratings", fakesonic.Library(10))
	r.check(t)
	r.sl.mu.Lock()
	r.sl.up = -1 // the server was lost meanwhile
	r.sl.mu.Unlock()
	r.check(t)
	if r.calls("search3") != 1 || r.calls("getPlaylists") != 2 || r.calls("getStarred2") != 1 {
		t.Fatalf("an unchanged server walked again: %v", r.f.Calls)
	}
	r.f.Mu.Lock()
	r.f.Songs = slices.Delete(r.f.Songs, 2, 3)
	r.f.Count, r.f.LastScan = 9, "2026-11-14T02:00:00Z"
	r.f.Songs[0].Starred = "2026-11-14T03:00:00Z"
	r.f.Mu.Unlock()
	batteryStatus = filepath.Join(r.tmp, "battery")
	t.Cleanup(func() { batteryStatus = "/sys/class/power_supply/battery/status" })
	os.WriteFile(batteryStatus, []byte("Charging\n"), 0o644)
	r.sl.mu.Lock()
	r.sl.up = -1
	r.sl.mu.Unlock()
	r.check(t)
	if r.calls("search3") != 2 {
		t.Fatalf("a changed hint while charging didn't walk: %v", r.f.Calls)
	}
	l := catLines(t, r.file(catalogue.CatName))
	if l[0] != fmt.Sprintf("#Q2CAT/1\tseq=2\taccount=%s\trows=9", r.acct()) {
		t.Fatal(l[0])
	}
	var gone catalogue.Song
	r.store().Each(r.acct(), func(s catalogue.Song) error {
		if s.ID == "s3" {
			gone = s
		}
		return nil
	})
	if !gone.Gone {
		t.Fatalf("s3: %+v", gone)
	}
	if in := catLines(t, r.file(catalogue.InName)); !strings.HasPrefix(in[len(in)-1], catalogue.ListenKey(catalogue.FileURL(r.card, "subsonic", "al0", "s1", "flac"))+"\ts1\t5\t") {
		t.Fatal(in)
	}
}

// A changed hint waits while the Q2 is busy and not charging; Resync Library walks at once.
func TestWalkWaitsForIdle(t *testing.T) {
	r := newCatRig(t, "playlist catalogue", fakesonic.Library(4))
	r.check(t)
	batteryStatus = filepath.Join(r.tmp, "battery")
	t.Cleanup(func() { batteryStatus = "/sys/class/power_supply/battery/status" })
	os.WriteFile(batteryStatus, []byte("Discharging\n"), 0o644)
	r.f.Mu.Lock()
	r.f.LastScan = "2026-11-14T05:00:00Z"
	r.f.Mu.Unlock()
	r.stream.start(songs(3, "flac"), 0) // a list plays: not idle
	r.sl.mu.Lock()
	r.sl.up = -1
	r.sl.mu.Unlock()
	r.check(t)
	r.check(t)
	if r.calls("search3") != 1 || !r.sl.due {
		t.Fatalf("walked while busy: %v", r.f.Calls)
	}
	r.slResync()
	r.check(t)
	if r.calls("search3") != 2 || r.sl.due {
		t.Fatalf("Resync Library didn't walk: %v", r.f.Calls)
	}
}

// Only a lost connection is server=down; a refused sign-in is shown, and the server stays up.
func TestServerUpDown(t *testing.T) {
	r := newCatRig(t, "playlist catalogue", fakesonic.Library(2))
	r.check(t)
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.check(t)
	if !strings.Contains(r.state(), "server=down\n") {
		t.Fatal(r.state())
	}
	if row, _ := find(r.settingsOp("settings"), "Resync Library"); row.Subtitle != "Server not answering" {
		t.Fatal(row.Subtitle)
	}
	r.f.Mu.Lock()
	r.f.Down, r.f.Pwd = false, "changed"
	r.f.Mu.Unlock()
	r.check(t)
	if !strings.Contains(r.state(), "server=up\n") {
		t.Fatal(r.state())
	}
	if row, _ := find(r.settingsOp("settings"), "Resync Library"); row.Subtitle != "Wrong username or password" {
		t.Fatal(row.Subtitle)
	}
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.slPinged(r.client().Ping(context.Background()))
	if !strings.Contains(r.state(), "server=down\n") {
		t.Fatal("a playback ping's lost server", r.state())
	}
}

// ratings.out goes to the server at the connection, before anything is read back, and is gone
// once sent; the walk then sends nothing back for it.
func TestRatingsOut(t *testing.T) {
	r := newCatRig(t, "playlist catalogue ratings", fakesonic.Library(3))
	r.check(t)
	synced := filepath.Join(r.card, "Subsonic", "A", "01 - x.flac")
	r.man = nil
	os.MkdirAll(filepath.Dir(synced), 0o755)
	os.WriteFile(r.file("manifest.json"), []byte(`{"Songs":{"s3":{"ID":"s3","Path":"`+synced+`"}}}`), 0o644)
	out := fmt.Sprintf("k\ts1\t5\t%d\n%s\t\t2\t%d\n", time.Now().Unix(), catalogue.ListenKey(synced), time.Now().Unix())
	os.WriteFile(r.file(catalogue.OutName), []byte(out), 0o644)
	os.Remove(r.file(catalogue.InName))
	r.sl.mu.Lock()
	r.sl.up = -1
	r.sl.mu.Unlock()
	r.check(t)
	if want := []string{"setRating s1 5", "star s1", "setRating s3 2"}; !slices.Equal(r.f.Writes, want) {
		t.Fatal(r.f.Writes)
	}
	for _, n := range []string{catalogue.OutName, catalogue.OutName + ".sending", catalogue.InName} {
		if _, err := os.Stat(r.file(n)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is there", n)
		}
	}
	r.slResync()
	r.check(t)
	if _, err := os.Stat(r.file(catalogue.InName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the walk sent the Q2's own hearts back: %q", catLines(t, r.file(catalogue.InName)))
	}
	// A lost server keeps them for the next connection.
	os.WriteFile(r.file(catalogue.OutName), []byte(fmt.Sprintf("k\ts2\t1\t%d\n", time.Now().Unix())), 0o644)
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.slConnect(r.client())
	if l := catLines(t, r.file(catalogue.OutName+".sending")); len(l) != 1 || !strings.HasPrefix(l[0], "k\ts2\t1\t") {
		t.Fatal(l)
	}
}

// playlists.out (the firmware's "playlists") goes to the server while it answers: a playlist's last
// set alone, a rename, a delete that drops the earlier lines of its playlist; the catalogue written
// after it has the server's new playlists. Without the word the file is left alone.
func TestPlaylistsOut(t *testing.T) {
	r := newCatRig(t, "playlist catalogue ratings playlists", fakesonic.Library(6))
	r.f.Playlists = []fakesonic.Playlist{{ID: "p1", Name: "Mix", Changed: "t1", SIDs: []string{"s1", "s2"}},
		{ID: "p2", Name: "Old", Changed: "t1", SIDs: []string{"s3"}}, {ID: "p3", Name: "Gone", Changed: "t1"}}
	r.check(t)
	r.f.Writes = nil
	out := "set\tp1\ts1\ns2\nset\tp1\ts2\ts4\ts1\nname\tp2\tNew%09Name\nset\tp3\ts5\ndelete\tp3\nset\tp9\ts1\n"
	out = strings.ReplaceAll(out, "\ns2\n", "\n") // a torn line is no change
	os.WriteFile(r.file(catalogue.PlsOutName), []byte(out), 0o644)
	r.check(t) // pending: the connection's checks run again
	want := []string{"createPlaylist p1 s2,s4,s1", "updatePlaylist p2 name=New\tName", "deletePlaylist p3"}
	if !slices.Equal(r.f.Writes, want) {
		t.Fatalf("%q", r.f.Writes)
	}
	for _, n := range []string{catalogue.PlsOutName, catalogue.PlsOutName + ".sending"} {
		if _, err := os.Stat(r.file(n)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is there", n)
		}
	}
	cat := strings.Join(catLines(t, r.file(catalogue.CatName)), "\n")
	if !strings.Contains(cat, "p\tp2\tNew%09Name") || strings.Contains(cat, "\tp3\t") ||
		!strings.Contains(cat, "i\tp1\ts4\t") {
		t.Fatalf("the catalogue's playlists:\n%s", cat)
	}
	// A lost server keeps the lines for the next connection.
	os.WriteFile(r.file(catalogue.PlsOutName), []byte("name\tp1\tLater\n"), 0o644)
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.slConnect(r.client())
	if l := catLines(t, r.file(catalogue.PlsOutName+".sending")); len(l) != 1 || l[0] != "name\tp1\tLater" {
		t.Fatal(l)
	}
	// An app without the firmware's word leaves the file as it is.
	q := newCatRig(t, "playlist catalogue ratings", fakesonic.Library(2))
	q.f.Playlists = []fakesonic.Playlist{{ID: "p1", Name: "Mix", Changed: "t1", SIDs: []string{"s1"}}}
	os.MkdirAll(q.dir, 0o755)
	os.WriteFile(q.file(catalogue.PlsOutName), []byte("name\tp1\tX\n"), 0o644)
	q.check(t)
	if q.calls("updatePlaylist") != 0 || len(catLines(t, q.file(catalogue.PlsOutName))) != 1 {
		t.Fatal(q.f.Writes)
	}
}

// A long playlist goes in chunks: createPlaylist with the first 200 songs, updatePlaylist adds the
// rest, so no request outgrows a server's URL limit; an emptied one is set to no songs.
func TestPlaylistsOutLong(t *testing.T) {
	r := newCatRig(t, "playlist catalogue playlists", fakesonic.Library(250))
	r.f.Playlists = []fakesonic.Playlist{{ID: "p1", Name: "Mix", Changed: "t1"}, {ID: "p2", Name: "Two", Changed: "t1", SIDs: []string{"s1"}}}
	r.check(t)
	r.f.Writes = nil
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = fmt.Sprintf("s%d", i+1)
	}
	os.WriteFile(r.file(catalogue.PlsOutName), []byte("set\tp1\t"+strings.Join(ids, "\t")+"\nset\tp2\t\n"), 0o644)
	r.check(t)
	want := []string{"createPlaylist p1 " + strings.Join(ids[:200], ","), "updatePlaylist p1 add=" + strings.Join(ids[200:], ","),
		"createPlaylist p2 "}
	if !slices.Equal(r.f.Writes, want) || len(r.f.Playlists[0].SIDs) != 250 || len(r.f.Playlists[1].SIDs) != 0 {
		t.Fatalf("%d writes, %d and %d songs", len(r.f.Writes), len(r.f.Playlists[0].SIDs), len(r.f.Playlists[1].SIDs))
	}
}

// .catplay is answered with the usual .play: its seq, and a queue of the server rows (streamed)
// and the card files as they are.
func TestCatplay(t *testing.T) {
	r := newCatRig(t, "playlist catalogue", fakesonic.Library(3))
	r.check(t)
	own := filepath.Join(r.card, "Music", "mine.flac")
	os.MkdirAll(filepath.Dir(own), 0o755)
	os.WriteFile(own, []byte("x"), 0o644)
	row := func(sid string) string { return catalogue.FileURL(r.card, "subsonic", "al0", sid, "flac") }
	req := strings.Join([]string{"42 1", row("s1"), own, row("s2"), row("s404")}, "\n") + "\n"
	os.WriteFile(r.catplayPath(), []byte(req), 0o644)
	r.slTick()
	r.bg.Wait()
	var play string
	eventually(t, ".play", func() bool {
		b, _ := os.ReadFile(filepath.Join(r.tmp, "q2-subsonic.play"))
		play = string(b)
		return play != ""
	})
	seq, m3u, _ := strings.Cut(play, " ")
	if seq != "42" {
		t.Fatal(play)
	}
	l := slices.DeleteFunc(catLines(t, m3u), func(s string) bool { return strings.HasPrefix(s, "#") })
	if len(l) != 2 || l[0] != own || !strings.Contains(l[1], "[s2].flac") {
		t.Fatalf("queue from the pressed row: %q", l)
	}
	r.stream.stop()
	os.Remove(filepath.Join(r.tmp, "q2-subsonic.play"))
	r.slTick() // the same request is answered once
	r.bg.Wait()
	if _, err := os.Stat(filepath.Join(r.tmp, "q2-subsonic.play")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("answered twice")
	}
}

// Show in Library off takes away what the firmware reads; Sign Out the app's records too.
func TestShowInLibraryOffAndSignOut(t *testing.T) {
	r := newCatRig(t, "playlist catalogue", fakesonic.Library(2))
	r.check(t)
	if m := r.settingsOp("showlib").Message; m != "Show in Library: Off" {
		t.Fatal(m)
	}
	if _, err := os.Stat(r.file(catalogue.CatName)); !errors.Is(err, os.ErrNotExist) || strings.Contains(r.state(), "server=") {
		t.Fatal("left on", r.state())
	}
	if _, err := os.Stat(r.file(catalogue.SongsName)); err != nil {
		t.Fatal("the app's records went", err)
	}
	r.settingsOp("showlib")
	r.check(t)
	if _, err := os.Stat(r.file(catalogue.CatName)); err != nil {
		t.Fatal("not written again", err)
	}
	r.signOut()
	r.bg.Wait()
	for _, n := range []string{catalogue.CatName, catalogue.SongsName, catalogue.PlaylistsName} {
		if _, err := os.Stat(r.file(n)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s kept after Sign Out", n)
		}
	}
}

// catalogue.avail follows the stream cache: a song kept, a song evicted.
func TestAvailFollowsCache(t *testing.T) {
	r := newCatRig(t, "playlist catalogue", fakesonic.Library(2))
	r.check(t)
	p := r.stream.cachePath(r.stream.cacheBase(), subsonic.Song{ID: "s1", Title: "T"}, "flac", "original")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("x"), 0o644)
	r.slAvail(p, true)
	r.stream.clearCache()
	if l := catLines(t, r.file(catalogue.AvailName)); !slices.Equal(l, []string{"s1\t" + p, "-s1"}) {
		t.Fatal(l)
	}
}

// A catalogue queue drops the songs not on the card when the server is lost, so the firmware
// plays on past them; the card's files and the cache's copies stay.
func TestCatQueueDropsUncachedWhenLost(t *testing.T) {
	e := newEnv(t, fw.Caps{Playlist: true})
	e.s.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	var pinged atomic.Int32
	e.s.pinged = func(error) { pinged.Add(1) }
	lost := &subsonic.APIError{Op: "stream", Msg: "connection refused"}
	e.src.err = errFor(lost, "s1")
	e.src.ping = func() error { return lost }
	own := filepath.Join(e.root, "Music", "own.flac")
	cached := filepath.Join(e.root, ".subsonic", "stream", "a", "original", "0", "t3 [s3].flac")
	for _, p := range []string{own, cached} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	sg := songs(5, "flac")
	e.s.startCat(sg, []string{"", "", own, "", ""}, 0, &catPlay{seq: 77, local: map[string]string{"s3": cached}})
	until(t, "the queue dropped what isn't on the card", func() bool {
		_, body := e.queue(t)
		return !strings.Contains(body, "[s1]")
	})
	_, body := e.queue(t)
	want := strings.Join([]string{e.stream(0, "flac"), own, cached}, "\n")
	if !strings.Contains(body, want) || strings.Contains(body, "[s4]") {
		t.Fatalf("queue %q", body)
	}
	if b, _ := os.ReadFile(e.play); !strings.HasPrefix(string(b), "77 ") {
		t.Fatalf(".play %q", b)
	}
	if pinged.Load() == 0 {
		t.Fatal("the ping wasn't told")
	}
}

func testJPEG(t *testing.T, px int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, px, px)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Once the catalogue is written, the checks after it fetch its albums' covers into covers/, a
// baseline JPEG each, named by album id; an album the server has none for, or one too large to
// show, is noted and never asked for again.
func TestCatalogueCovers(t *testing.T) {
	r := newCatRig(t, "catalogue", fakesonic.Library(30)) // ten albums, al0 to al9
	r.f.Mu.Lock()
	r.f.Covers = map[string][]byte{}
	for i := range 8 {
		r.f.Covers[fmt.Sprintf("al%d", i)] = testJPEG(t, 160)
	}
	r.f.Covers["al9"] = testJPEG(t, 400) // past CoverPx: not one the firmware is given
	r.f.Mu.Unlock()                      // al8: none on the server
	r.check(t)                           // the first copy and the catalogue
	if n := r.calls("getCoverArt"); n != 0 {
		t.Fatal("covers before the catalogue's first check is over:", n)
	}
	r.check(t)
	dir := r.file(catalogue.CoversName)
	for i := range 10 {
		_, err := os.Stat(filepath.Join(dir, fmt.Sprintf("al%d.jpg", i)))
		if (err == nil) != (i < 8) {
			t.Fatalf("al%d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "al0.jpg"))
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(b)); err != nil || cfg.Width != 160 || !bytes.HasPrefix(b, jfif) {
		t.Fatal(cfg, err)
	}
	if got := catLines(t, filepath.Join(dir, ".none")); !slices.Equal(sorted(got), []string{"al8", "al9"}) {
		t.Fatal(got)
	}
	r.check(t)
	if n := r.calls("getCoverArt"); n != 10 {
		t.Fatal("getCoverArt calls:", n)
	}
	// Show in Library off: the covers go with the catalogue
	r.slToggle(r.settings(), "off")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func sorted(s []string) []string { slices.Sort(s); return s }

// Covers are fetched while a list plays what it already fetched (the stream is active but moves
// no data), and wait while a song of the stream is still to fetch.
// A server playlist's cover is covers/pl-<id>.jpg, asked for by its coverArt id after the albums',
// and goes with the playlist.
func TestPlaylistCovers(t *testing.T) {
	r := newCatRig(t, "catalogue", fakesonic.Library(3))
	r.f.Mu.Lock()
	r.f.Playlists = []fakesonic.Playlist{{ID: "p1", Name: "Mix", Changed: "t1", CoverArt: "pl-p1_5", SIDs: []string{"s1"}},
		{ID: "p2", Name: "Bare", Changed: "t1", SIDs: []string{"s2"}}}
	r.f.Covers = map[string][]byte{"pl-p1_5": testJPEG(t, 160)}
	r.f.Mu.Unlock()
	batteryStatus = filepath.Join(r.tmp, "battery")
	t.Cleanup(func() { batteryStatus = "/sys/class/power_supply/battery/status" })
	os.WriteFile(batteryStatus, []byte("Charging\n"), 0o644)
	r.check(t) // the first copy
	r.check(t) // the covers' round
	if _, err := os.Stat(filepath.Join(r.dir, catalogue.CoversName, "pl-p1.jpg")); err != nil {
		t.Fatal(err, r.f.Calls)
	}
	if _, err := os.Stat(filepath.Join(r.dir, catalogue.CoversName, "pl-p2.jpg")); err == nil {
		t.Fatal("a cover for a playlist without one")
	}
	r.f.Mu.Lock()
	r.f.Playlists = r.f.Playlists[1:]
	r.f.Mu.Unlock()
	r.sl.mu.Lock()
	r.sl.up = -1
	r.sl.mu.Unlock()
	r.check(t) // the playlist gone: read again on the connection
	r.check(t)
	if _, err := os.Stat(filepath.Join(r.dir, catalogue.CoversName, "pl-p1.jpg")); err == nil {
		t.Fatal("a gone playlist's cover kept")
	}
}

func TestCoversWhileAListPlays(t *testing.T) {
	r := newCatRig(t, "catalogue", fakesonic.Library(30)) // ten albums
	r.f.Mu.Lock()
	r.f.Covers = map[string][]byte{"al0": testJPEG(t, 160), "al1": testJPEG(t, 160)}
	r.f.Mu.Unlock()
	batteryStatus = filepath.Join(r.tmp, "battery")
	t.Cleanup(func() { batteryStatus = "/sys/class/power_supply/battery/status" })
	os.WriteFile(batteryStatus, []byte("Discharging\n"), 0o644)
	r.check(t) // the first copy and the catalogue
	// a list plays, its window fetched: the stream's run, standing still
	done := make(chan struct{})
	r.stream.mu.Lock()
	r.stream.done, r.stream.cancel = done, func() { close(done) }
	r.stream.mu.Unlock()
	r.stream.xfer.Store(true) // a song of it still to fetch
	r.check(t)
	if n := r.calls("getCoverArt"); n != 0 || !r.stream.active() {
		t.Fatal("covers while the stream fetches:", n)
	}
	r.stream.xfer.Store(false)
	r.check(t)
	if n := r.calls("getCoverArt"); n != 10 || !r.stream.active() {
		t.Fatal("covers while the list plays from what it fetched:", n)
	}
}

// source sets the firmware's Streaming Songs switch as Q2 Pod with "libsource" does: "server",
// "local", or "" for no q2-addon.library at all.
func (r *catRig) source(t *testing.T, s string) {
	t.Helper()
	p := filepath.Join(r.tmp, "q2-addon.library")
	if s == "" {
		os.Remove(p)
		return
	}
	if err := os.WriteFile(p, []byte(s+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *catRig) allCalls() int {
	r.f.Mu.Lock()
	defer r.f.Mu.Unlock()
	n := 0
	for _, c := range r.f.Calls {
		n += c
	}
	return n
}

// Show in Library is On only while it runs: on firmware with "libsource", only while
// q2-addon.library says "server". Off there, the row is "needslib"; firmware without the word,
// which knows no such row, gets an action row, as before.
func TestShowInLibraryFollowsStreamingSongs(t *testing.T) {
	for _, c := range []struct {
		caps, source string
		saved        bool
		kind, title  string
		sub          string
	}{
		{"playlist catalogue", "", true, "action", "Show in Library: On", ""},
		{"playlist catalogue", "local", true, "action", "Show in Library: On", ""}, // no word: the file means nothing
		{"playlist catalogue", "", false, "action", "Show in Library: Off", "Streaming songs beside the card's"},
		{"playlist catalogue libsource", "server", true, "action", "Show in Library: On", ""},
		{"playlist catalogue libsource", "local", true, "needslib", "Show in Library: Off", "Streaming Songs is off"},
		{"playlist catalogue libsource", "", true, "needslib", "Show in Library: Off", "Streaming Songs is off"},
		{"playlist catalogue libsource", "server", false, "needslib", "Show in Library: Off", "Streaming songs beside the card's"},
		{"playlist catalogue libsource", "local", false, "needslib", "Show in Library: Off", "Streaming songs beside the card's"},
	} {
		r := newCatRig(t, c.caps, fakesonic.Library(2))
		r.saveSettings(settings{Library: c.saved})
		r.source(t, c.source)
		r.slInit()
		row, ok := find(r.settingsOp("settings"), "Show in Library")
		if !ok || row.Kind != c.kind || row.Title != c.title || row.URI != "showlib" ||
			c.sub != "" && row.Subtitle != c.sub {
			t.Errorf("%q, file %q, saved %v: %+v", c.caps, c.source, c.saved, row)
		}
		if r.slActive() != (c.title == "Show in Library: On") {
			t.Errorf("%q, file %q, saved %v: slActive %v", c.caps, c.source, c.saved, r.slActive())
		}
	}
}

// Streaming Songs off pauses the Server Library: no request to the server, and its files and
// Show in Library stay. On again, the server is asked at once, without a new first copy, and the
// catalogue is written again.
func TestStreamingSongsPausesAndResumes(t *testing.T) {
	r := newCatRig(t, "playlist catalogue libsource", fakesonic.Library(5))
	r.source(t, "server")
	r.check(t)
	if r.calls("search3") != 1 || r.calls("ping") == 0 {
		t.Fatalf("no first copy: %v", r.f.Calls)
	}
	seq := catalogue.Seq(r.file(catalogue.CatName))
	r.source(t, "local")
	before := r.allCalls()
	for range 3 {
		r.sl.mu.Lock()
		r.sl.up = -1 // a reconnection would check everything
		r.sl.mu.Unlock()
		r.check(t)
	}
	if n := r.allCalls(); n != before {
		t.Fatalf("paused, %d requests: %v", n-before, r.f.Calls)
	}
	for _, n := range []string{catalogue.CatName, catalogue.SongsName} {
		if _, err := os.Stat(r.file(n)); err != nil {
			t.Fatalf("paused, %s went: %v", n, err)
		}
	}
	if !r.settings().Library || !r.sl.on.Load() {
		t.Fatal("paused, Show in Library was saved off")
	}
	r.source(t, "server")
	r.check(t)
	if r.calls("ping") == 0 || r.allCalls() == before {
		t.Fatalf("resumed, no request: %v", r.f.Calls)
	}
	if r.calls("search3") != 1 {
		t.Fatalf("resumed with a new first copy: %v", r.f.Calls)
	}
	if got := catalogue.Seq(r.file(catalogue.CatName)); got <= seq {
		t.Fatalf("catalogue not written again: seq %d, was %d", got, seq)
	}
	if row, _ := find(r.settingsOp("settings"), "Show in Library"); row.Title != "Show in Library: On" {
		t.Fatal(row)
	}
}

// press is a press of Settings' Show in Library row as the firmware sends it: its op and context.
func (r *catRig) press(t *testing.T) q2spot.Reply {
	t.Helper()
	row, ok := find(r.settingsOp("settings"), "Show in Library")
	if !ok {
		t.Fatal("no Show in Library row")
	}
	return r.settingsOp(row.URI, row.Context)
}

// A "needslib" press arrives once the firmware has turned Streaming Songs on: it is Show in
// Library on, whether it was saved off, or saved on and waiting for the switch. A tick that sees
// the switch before the press changes nothing.
func TestNeedslibPress(t *testing.T) {
	for _, tick := range []bool{false, true} {
		r := newCatRig(t, "playlist catalogue libsource", fakesonic.Library(2))
		r.source(t, "local")
		r.slInit()
		row, _ := find(r.settingsOp("settings"), "Show in Library")
		r.source(t, "server") // the firmware's Turn On, then the press
		if tick {
			r.slTick()
		}
		if m := r.settingsOp(row.URI, row.Context).Message; m != "Show in Library: On" || !r.settings().Library || !r.slActive() {
			t.Fatalf("tick %v: %q, saved %v, active %v", tick, m, r.settings().Library, r.slActive())
		}
		if m := r.press(t).Message; m != "Show in Library: Off" || r.settings().Library {
			t.Fatalf("tick %v: %q, saved %v", tick, m, r.settings().Library)
		}
		if m := r.press(t).Message; m != "Show in Library: On" || !r.slActive() {
			t.Fatalf("tick %v: saved off: %q", tick, m)
		}
		r.bg.Wait()
	}
}
