package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/creds"
	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
	"github.com/andsuor/q2-subsonic/internal/tlsroots"
)

const (
	answerIn    = 8 * time.Second  // every Q2SPOT/1 reply within this (the firmware gives up at 15 s)
	idleAfter   = 30 * time.Minute // exit 0 after this long with no request, sync or stream
	signinPort  = 8080             // or the next free one
	previewTTL  = time.Minute      // a preview older than this is computed again when Sync is opened
	pingEvery   = 30 * time.Second
	qrLeftAfter = 5 * time.Second // a gap this long in the signed-out menu's 2 s refresh: the QR page was open (no "signin" word)
	browseTTL   = time.Minute     // a list fetched for browsing serves its pages, and a song pressed on it, this long
	cacheMax    = 4               // lists kept at most: the newest
)

var errSignedOut = errors.New("signed out")

// settings is .subsonic/settings.json. StreamQuality "" follows DefaultQuality. NoReport turns Report Plays off. StreamCacheMB is the stream
// cache's limit: 0 is 2 GB, -1 Off. Home is the server's home address (home.go), set by the phone sign-in.
type settings struct {
	DefaultQuality string `json:"defaultQuality,omitempty"`
	StreamQuality  string `json:"streamQuality,omitempty"`
	NoReport       bool   `json:"noReport,omitempty"`
	StreamCacheMB  int    `json:"streamCacheMB,omitempty"`
	Home           string `json:"home,omitempty"`             // the server's home address (home.go)
	LimitMB        int    `json:"downloadsLimitMB,omitempty"` // Downloads' limit in MB, 0 for none (storage.go)
}

// cacheSizes are Stream Cache's steps in MB, the firmware's Spotify Cache sizes, and Off.
var cacheSizes = []int{512, 1024, 2048, 4096, 8192, -1}

func (s settings) cacheMB() int {
	if s.StreamCacheMB == 0 {
		return 2048
	}
	return s.StreamCacheMB
}

func (s settings) cacheBytes() int64 { return int64(max(s.cacheMB(), 0)) << 20 }

func (s settings) cacheName() string {
	switch mb := s.cacheMB(); {
	case mb < 0:
		return "Stream Cache: Off"
	case mb%1024 == 0:
		return fmt.Sprintf("Stream Cache: %d GB", mb/1024)
	default:
		return fmt.Sprintf("Stream Cache: %d MB", mb)
	}
}

func (s settings) reportName() string {
	if s.NoReport {
		return "Report Plays: Off"
	}
	return "Report Plays: On"
}

func (s settings) def() string {
	if s.DefaultQuality == "" {
		return "original"
	}
	return s.DefaultQuality
}

func (s settings) stream() string {
	if s.StreamQuality == "" {
		return s.def()
	}
	return s.StreamQuality
}

// stateKeys is the state file: sign-in's QR keys, hold=1, version, the Server Library's
// server=up or down, what the app syncs (sync=, syncState), the server's reach (reach=, srverr=,
// reach.go) and the answers to the firmware's requests (radioack= and the other *ack= keys,
// offlineack= among them), merged so no writer drops another's keys.
type stateKeys struct {
	st     *q2.State
	mu     sync.Mutex
	sign   map[string]string
	hold   bool
	server string // "" writes no server key
	sync   string // "" writes no sync key
	radio  string // the radio's seed title (radio.go); "" writes no radio key
	ack    string // radioack=: the last .radio request answered, "<seq> <code>"; "" none
	lyrAck string // lyricsack=: the last .lrcask request answered, "<seq> <code>"; "" none
	covAck string // coverack=: the last .coverask line answered, "<seq> <code>"; "" none
	resAck string // resumeack=: the last .resume request answered, "<seq> <code>"; "" none
	offAck string // offlineack=: the last .offline request answered, "<seq> <words>" (offline.go); "" none
	reach  string // reach=: "down <code>" while the server is out of reach (reach.go); "" none
	srvErr string // srverr=: the last server failure to toast, "<unix> <code>" (reach.go); "" none
}

func (s *stateKeys) setOfflineAck(v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offAck = v
	return s.flush()
}

// setReach sets reach= ("" takes it out) and, unless srvErr is "", srverr=.
func (s *stateKeys) setReach(reach, srvErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reach = reach
	if srvErr != "" {
		s.srvErr = srvErr
	}
	return s.flush()
}

// reachNow is reach= as it stands.
func (s *stateKeys) reachNow() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reach
}

func (s *stateKeys) setResumeAck(v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resAck = v
	return s.flush()
}

func (s *stateKeys) setCoverAck(v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.covAck = v
	return s.flush()
}

func (s *stateKeys) setLyricsAck(v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lyrAck = v
	return s.flush()
}

func (s *stateKeys) setRadio(v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.radio = v
	return s.flush()
}

func (s *stateKeys) setRadioAck(v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ack = v
	return s.flush()
}

func (s *stateKeys) setSync(v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sync = v
	return s.flush()
}

func (s *stateKeys) setServer(v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.server = v
	return s.flush()
}

func (s *stateKeys) Write(kv map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sign = kv
	return s.flush()
}

func (s *stateKeys) setHold(h bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = h
	return s.flush()
}

func (s *stateKeys) remove() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Remove()
}

func (s *stateKeys) flush() error {
	m := map[string]string{"version": version}
	for k, v := range s.sign {
		m[k] = v
	}
	if s.hold {
		m["hold"] = "1"
	}
	if s.server != "" {
		m["server"] = s.server
	}
	if s.sync != "" {
		m["sync"] = s.sync
	}
	if s.radio != "" {
		m["radio"] = s.radio
	}
	if s.ack != "" {
		m["radioack"] = s.ack
	}
	if s.lyrAck != "" {
		m["lyricsack"] = s.lyrAck
	}
	if s.covAck != "" {
		m["coverack"] = s.covAck
	}
	if s.resAck != "" {
		m["resumeack"] = s.resAck
	}
	if s.offAck != "" {
		m["offlineack"] = s.offAck
	}
	if s.reach != "" {
		m["reach"] = s.reach
	}
	if s.srvErr != "" {
		m["srverr"] = s.srvErr
	}
	return s.st.Write(m)
}

type preview struct {
	gen     int
	at      time.Time
	done    bool
	plan    *plan.Plan
	err     error
	cancel  context.CancelFunc // stops its fetches (dropPreview)
	full    bool               // started by Check Again: the library cache is not used
	note    string             // how far the check has come, such as "4500 of 10000 songs"; under daemon.mu
	reclaim int64              // bytes of the plan's free space that are the stream cache's to give
	over    int64              // bytes the plan would take the downloads past their limit (storage.go)
	shown   time.Time          // when a status Sync page last showed it done (sync_status.go reopenAfter)
}

type libJob struct {
	seq    int64
	done   bool
	ok     bool
	msg    string
	doneAt time.Time
}

// cacheEntry is one list as the server answered it (never rows: every page's subtitles are built
// from the selection and the card as they are), for the client that asked.
type cacheEntry struct {
	key string
	cl  *subsonic.Client
	at  time.Time
	v   any
}

// cached is fetch's answer for key, fetched at most once per browseTTL and client: the firmware
// asks for a list 50 rows at a time, and a pressed song asks for its list again. d.mu is never
// held across fetch, which may take seconds. A failure is not kept.
// ponytail: bounded by count, not bytes: four lists of a huge server are a few MB for a minute.
func cached[T any](d *daemon, cl *subsonic.Client, key string, fetch func() (T, error)) (T, error) {
	d.mu.Lock()
	for _, e := range d.cache {
		if v, ok := e.v.(T); ok && e.key == key && e.cl == cl && time.Since(e.at) < d.browseTTL {
			d.mu.Unlock()
			return v, nil
		}
	}
	d.mu.Unlock()
	v, err := fetch()
	if err != nil {
		return v, err
	}
	d.mu.Lock()
	d.cache = slices.DeleteFunc(d.cache, func(e cacheEntry) bool { return e.key == key })
	if len(d.cache) >= cacheMax {
		d.cache = slices.Delete(d.cache, 0, len(d.cache)-cacheMax+1)
	}
	d.cache = append(d.cache, cacheEntry{key, cl, time.Now(), v})
	d.mu.Unlock()
	return v, nil
}

// songList is the songs of Top Songs, a search or an artist's All Songs as last shown (50 at most
// but All Songs'), so a song pressed there plays on through them. Its key is the rows' list key:
// "top:<artist id>", "q:" or "songs:<artist id>".
type songList struct {
	key   string
	songs []subsonic.Song
}

// daemon is the running app: the Q2SPOT/1 handler (device) and what it drives.
type daemon struct {
	dir, card, tmp string // the app folder (.subsonic), the card's root, /tmp
	caps           fw.Caps
	log            *applog.Log
	state          *stateKeys
	signin         *signin
	runner         *syncer.Runner
	stream         *streamer
	art            *artCache
	newClient      func(subsonic.Creds) (*subsonic.Client, error)
	free           func(path string) int64
	addrs          func() []net.Addr // the Q2's interfaces' addresses for the home address (home.go); nil is localAddrs
	answerIn       time.Duration
	signinPort     int

	mu                 sync.Mutex
	cl                 *subsonic.Client
	server, user, host string
	lastReq, lastMenu  time.Time
	pingAt             time.Time
	pingErr            error
	pinging, pinged    bool
	gen                int // bumped by every selection change
	preview            *preview
	showDone           bool // the finished sync's page, until Check Again, a new Start, or a choice made after it ended
	fullCheck          bool // Check Again was pressed: the next preview checks the whole library
	cardWalked         bool // the card was walked for deleted songs since the app started
	lib                *libJob
	libTaken           int64                       // the library request whose answer was taken last (libPoll)
	libAgain           bool                        // a request was wanted while one awaited its answer: sent once that comes
	libRest            [2][]string                 // the import and add paths the request on file had no room for (I20)
	resolved           map[string][]subsonic.Song  // selection key -> its songs, as last fetched
	artistAlbums       map[string][]subsonic.Album // artist id -> its albums, as last fetched
	gone               map[string]bool             // chosen item keys the server answered "not found" for, as last previewed
	libCount           [2]int                      // albums, playlists of the Entire Library, as last resolved
	playSeq            int                         // the latest play request; an older one still waiting is dropped
	playMu             sync.Mutex                  // one streamer.start at a time
	man                *library.Manifest
	manAt              time.Time
	manSize            int64
	migrateErr         error     // the one-time Navidrome/ -> Subsonic/ move failed; Sync shows it
	migrateAt          time.Time // when it ran
	wasBusy            bool      // the runner, at the last tick
	cache              []cacheEntry
	browseTTL          time.Duration
	listed             songList      // the song list shown last
	clearAt            time.Time     // Clear Cache's first press: a second one soon after clears
	sl                 serverLib     // the Server Library (catalogue.go)
	qrSeen             bool          // the QR page's marker was there since the sign-in started (qrLeft)
	st                 stState       // Settings > Storage's files (storage.go)
	radio              radioState    // the radio playing and its recent songs (radio.go)
	lyr                lyricsState   // the firmware's lyrics requests (lyrics.go)
	cov                coverAskState // the car's cover requests (coverask.go)
	resume             resumeState   // the firmware's request to resume a restored stream (resume.go)
	reach              *reach        // whether the server can be reached, for the status row (reach.go)
	off                offlineState  // the firmware's Sync Offline requests (offline.go)

	bg sync.WaitGroup // the work replies leave running (ping, preview, play, sign-out's stops); tests wait for it
}

// spawn runs f in the background, counted in d.bg.
func (d *daemon) spawn(f func()) {
	d.bg.Add(1)
	go func() { defer d.bg.Done(); f() }()
}

func newDaemon(dir, card, tmp string) *daemon {
	os.MkdirAll(dir, 0o755)
	lg := applog.Open(filepath.Join(dir, "log.txt"), 1<<20)
	if err := lg.Scrub(); err != nil { // an older build's lines kept the user name of each request
		lg.Printf("log: scrub: %v", err)
	}
	d := &daemon{dir: dir, card: card, tmp: tmp, log: lg,
		caps:  fw.ReadCaps(filepath.Join(tmp, "q2-addon.caps")),
		state: &stateKeys{st: &q2.State{Path: filepath.Join(tmp, "q2-subsonic.state")}},
		newClient: func(c subsonic.Creds) (*subsonic.Client, error) {
			return subsonic.New(c, tlsroots.Client(15*time.Second))
		},
		free: freeBytes, answerIn: answerIn, signinPort: signinPort, browseTTL: browseTTL,
		lastReq: time.Now(), resolved: map[string][]subsonic.Song{}, artistAlbums: map[string][]subsonic.Album{}}
	d.reach = &reach{keys: d.state, now: time.Now}
	d.signin = &signin{dir: dir, state: d.state, addrs: func() []net.Addr {
		if d.addrs != nil {
			return d.addrs()
		}
		return localAddrs()
	}, newClient: func(c subsonic.Creds) (*subsonic.Client, error) {
		cl, err := d.newClient(c)
		if cl != nil {
			cl.Log = lg // the sign-in ping goes in log.txt too, redacted
		}
		return cl, err
	}}
	d.runner = syncer.New(card, src{d}, lg)
	d.runner.OnDone = d.syncDone
	d.stream = newStreamer(card, filepath.Join(tmp, "q2-subsonic.play"), filepath.Join(tmp, "q2-subsonic.playing"),
		d.caps, src{d}, d.manifest, syncer.Net, time.Second)
	d.stream.log = lg
	d.stream.aheadFile = filepath.Join(tmp, "q2-subsonic.ahead") // Download Ahead (ahead.go)
	d.stream.statusFile = filepath.Join(tmp, "q2-subsonic.stream")
	d.stream.retryFile = filepath.Join(tmp, "q2-subsonic.retry") // Now Playing's Retry
	d.stream.quality = func() string { return d.settings().stream() }
	d.stream.moving = d.runner.Moving
	// An older build's Streaming: Direct left its URL queue here, which carries the sign-in.
	os.Remove(filepath.Join(tmp, "q2-subsonic.urls"))
	os.Remove(filepath.Join(tmp, "q2-subsonic.urls.tmp"))
	d.stream.plays.send = src{d}.Scrobble
	d.stream.plays.on = func() bool { return !d.settings().NoReport }
	d.stream.plays.clock = func() time.Time {
		if !tlsroots.Known() { // then Now could wait for the server's time, and still not have it
			return time.Time{}
		}
		return tlsroots.Now()
	}
	d.stream.cacheCap = func() int64 { return d.settings().cacheBytes() }
	d.stream.acct = d.acct
	d.stream.free = func(p string) int64 { return d.free(p) }
	d.stream.clock = func() time.Time {
		if now, ok := libClock(); ok {
			return now
		}
		return time.Time{}
	}
	d.stream.reserve = func() int64 { // while a sync runs or waits, the cache leaves it its room
		if s := d.runner.Snapshot(); s.State == "running" || s.State == "paused" {
			return s.Room
		}
		return 0
	}
	d.runner.MakeRoom = d.stream.makeRoom
	d.stream.cleanLeft() // a stream an earlier run left (M26)
	// Counts what the cache keeps, and deletes what another account left: its songs can't play
	// for this one. In the background: a walk of the card.
	keep := ""
	if c, ok := creds.Load(dir); ok {
		keep = acctDir(c.Server, c.User)
	}
	d.stream.avail, d.stream.pinged = d.slAvail, d.slPinged // before the sweep, whose drops tell avail
	d.stream.gaveUp = d.reach.gaveUp
	d.slInit()
	d.radioInit()
	d.lyricsInit()
	d.resumeInit()
	d.offlineInit()
	d.stream.played = d.radioPlayed
	d.stream.started = func(radio bool) {
		if !radio {
			d.radioEnd() // another list: the radio's songs are gone from the queue
			return
		}
		d.radio.mu.Lock()
		d.radio.running = true
		d.radio.mu.Unlock()
	}
	d.spawn(func() { d.stream.sweep(keep) })
	d.art = newArtCache(d, filepath.Join(tmp, "q2-subsonic.art"))
	d.state.Write(nil)
	d.migrateErr, d.migrateAt = library.Migrate(card, lg.Printf), time.Now()
	d.logCrash()
	return d
}

// logCrash puts in log.txt what the launcher kept of a run that failed (/tmp/q2-subsonic.crash:
// its stderr, a panic's trace, and its exit code), so a crash or an OOM kill leaves a trace on the
// card (M43). A long trace keeps its start and the exit line.
func (d *daemon) logCrash() {
	f := filepath.Join(d.tmp, "q2-subsonic.crash")
	b, err := os.ReadFile(f)
	if err != nil {
		return
	}
	os.Remove(f)
	s := strings.TrimSpace(string(b))
	if len(s) > 4<<10 {
		s = strings.ToValidUTF8(s[:4<<10], "") + "\n…\n" + s[strings.LastIndex(s, "\n")+1:]
	}
	d.log.Printf("the last run failed:\n%s", s)
}

func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// client is the signed-in server client, loaded from the card's credentials; nil when signed out.
func (d *daemon) client() *subsonic.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cl != nil {
		return d.cl
	}
	c, ok := creds.Load(d.dir)
	if !ok {
		return nil
	}
	cl, err := d.newClient(c)
	if err != nil {
		return nil
	}
	cl.Log = d.log
	cl.Seen = d.reach.seen
	if h := loadSettings(d.dir).Home; h != "" {
		if err := cl.Home(h, nearHome(h, d.addrs)); err != nil {
			d.log.Printf("home address: %v", err) // settings.json edited by hand: the main address only
		}
	}
	d.cl, d.server, d.user, d.host = cl, c.Server, c.User, c.Server
	if u, err := url.Parse(c.Server); err == nil && u.Hostname() != "" {
		d.host = u.Hostname()
	}
	tlsroots.SetDateURL(c.Server)
	return cl
}

// acct names the signed-in account's folder in the stream cache; "" when signed out.
func (d *daemon) acct() string {
	if d.client() == nil {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return acctDir(d.server, d.user)
}

func (d *daemon) hostName() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.host
}

// signOut forgets the sign-in at once; the sync and stream stop in the background (R27: a reply
// never waits for a download to notice its cancel). The sync is cancelled, not paused: a later
// sign-in, maybe to another server, never resumes this account's plan (M12). Finished songs stay.
// The stream cache is emptied, as the firmware does with Spotify's, but for the list the Q2 may
// still play.
func (d *daemon) signOut() {
	creds.Remove(d.dir)
	d.reach.seen(nil) // no server to reach: the status row's mark goes
	d.mu.Lock()
	d.cl, d.pinged = nil, false
	d.cache, d.listed = nil, songList{}
	d.dropPreview()
	d.playSeq++ // a play still waiting for playMu is dropped
	d.mu.Unlock()
	// The account's whole library listing; the preview that could write it again is dropped above.
	if err := library.Remove(d.card, filepath.Join(d.card, ".subsonic", "library.json")); err != nil {
		d.log.Printf("sign out: library cache: %v", err)
	}
	d.slSignOut()
	d.radioEnd()
	d.radio.mu.Lock()
	d.radio.recent = nil // another account's songs
	d.radio.mu.Unlock()
	d.art.clear() // after the client is gone: a fetch that had it stores nothing
	d.spawn(d.runner.Cancel)
	d.spawn(func() { // under playMu, so a start already running finishes first and is then stopped
		d.playMu.Lock()
		d.stream.stop()
		d.playMu.Unlock() // the clear is a walk of the card, which no Play waits for
		d.stream.clearCache()
	})
}

// src lends the current client to the runner and the streamer, which outlive a sign-in.
type src struct{ d *daemon }

func (s src) MediaURL(id, q string) string {
	if c := s.d.client(); c != nil {
		return c.MediaURL(id, q)
	}
	return ""
}

func (s src) PlayURL(id, q string) string {
	if c := s.d.client(); c != nil {
		return c.PlayURL(id, q)
	}
	return ""
}

func (s src) CoverURL(id string) string {
	if c := s.d.client(); c != nil {
		return c.CoverURL(id)
	}
	return ""
}

func (s src) Scrobble(ctx context.Context, id string, at time.Time, submission bool) error {
	if c := s.d.client(); c != nil {
		return c.Scrobble(ctx, id, at, submission)
	}
	return errSignedOut
}

func (s src) Ping(ctx context.Context) error {
	if c := s.d.client(); c != nil {
		return c.Ping(ctx)
	}
	return errSignedOut
}

func (s src) Get(ctx context.Context, u string, from int64) (*http.Response, error) {
	if c := s.d.client(); c != nil {
		return c.Get(ctx, u, from)
	}
	return nil, errSignedOut
}

// manifest is the card's manifest, read again only when the file changed.
func (d *daemon) manifest() *library.Manifest {
	p := filepath.Join(d.card, ".subsonic", "manifest.json")
	st, err := os.Stat(p)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.man == nil || err != nil || !st.ModTime().Equal(d.manAt) || st.Size() != d.manSize {
		d.man, _ = library.LoadManifest(p)
		d.manAt, d.manSize = time.Time{}, 0
		if err == nil {
			d.manAt, d.manSize = st.ModTime(), st.Size()
		}
	}
	return d.man
}

func (d *daemon) syncedPath() string { return filepath.Join(d.card, ".subsonic", "synced") }

func (d *daemon) selPath() string { return filepath.Join(d.dir, "selection.json") }

func (d *daemon) selection() (selection.Set, error) { return selection.Load(d.selPath()) }

// saveSelection writes s. The finished sync's page gives way to the new preview, but not while a
// sync runs or is paused: its summary is still to come (M38).
func (d *daemon) saveSelection(s selection.Set) error {
	err := s.Save(d.selPath())
	if err == nil {
		d.writeOfflineList(s) // the firmware's Sync Offline rows read it (offline.go)
	}
	st := d.runner.Snapshot().State
	d.mu.Lock()
	d.gen++
	d.st.pending = true // Storage's items follow the selection
	if st != "running" && st != "paused" {
		d.showDone = false
	}
	d.mu.Unlock()
	return err
}

// resetSelection puts a damaged selection.json aside as selection.json.bad, so the user can start
// again on the Q2 (M21).
func (d *daemon) resetSelection() q2spot.Reply {
	if err := os.Rename(d.selPath(), d.selPath()+".bad"); err != nil && !errors.Is(err, os.ErrNotExist) {
		d.log.Printf("selection: %v", err)
		return q2spot.Fail(q2spot.Error, "Couldn't reset the selection")
	}
	d.mu.Lock()
	d.gen++
	d.mu.Unlock()
	return q2spot.Reply{Message: "Selection reset"}
}

// damagedSelection is a page when selection.json can't be read (M21).
func damagedSelection() q2spot.Reply {
	return q2spot.Reply{Rows: []q2spot.Row{info("Selection file is damaged"), act("resetsel", "Reset Selection", "")}}
}

const selDamaged = "Selection file is damaged: reset it in Downloads"

func (d *daemon) settings() settings { return loadSettings(d.dir) }

func (d *daemon) saveSettings(s settings) error { return storeSettings(d.dir, s) }

func loadSettings(dir string) settings {
	var s settings
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func storeSettings(dir string, s settings) error {
	b, _ := json.Marshal(s)
	return q2.WriteFile(filepath.Join(dir, "settings.json"), b)
}

// tick keeps the state file's hold=1 while files move (a sync, a stream fetching, a Server Library
// check) or the phone page is open, takes the firmware's answer to a library request as soon as it
// comes, and runs the Server Library's part (slTick).
// A run's end, however it ends (done, paused, stopped), counts as activity: its page stays as long
// as after a press (I15, review G3 Minor 8).
func (d *daemon) tick() {
	busy := d.runner.Busy()
	d.mu.Lock()
	if d.wasBusy && !busy {
		d.lastReq = time.Now()
	}
	d.wasBusy = busy
	d.cache = slices.DeleteFunc(d.cache, func(e cacheEntry) bool { return time.Since(e.at) >= d.browseTTL })
	d.mu.Unlock()
	if !d.runner.CardMounted() { // pulled: what comes back may have lost songs, or be another card
		d.mu.Lock()
		d.cardWalked = false
		d.mu.Unlock()
	}
	d.qrLeft()
	d.state.setHold(busy || d.signin.active() || d.stream.busy() || d.sl.busy.Load())
	d.state.setSync(d.syncState())
	d.slTick()
	d.stTick()
	d.radioTick()
	d.lyricsTick()
	d.coverTick()
	d.resumeTick()
	d.offlineTick()
	if d.caps.Library {
		d.mu.Lock()
		d.libPoll()
		d.mu.Unlock()
	}
}

// syncState is the state file's sync= for Q2 Pod's status row (its docs/internals.md#card-app-sync):
// "down DONE TOTAL" while a sync downloads songs (bytes, or songs before their sizes are known),
// "check" while a sync's check or the Server Library's connection checks or walk ask the server, ""
// while idle. The file is rewritten only when it changes (q2.State), so a download's progress
// writes it about once a tick.
func (d *daemon) syncState() string {
	if s := d.runner.Snapshot(); s.State == "running" {
		if s.BytesTotal > 0 {
			return fmt.Sprintf("down %d %d", s.BytesDone, s.BytesTotal)
		}
		return fmt.Sprintf("down %d %d", s.Done, s.Total)
	}
	d.mu.Lock()
	checking := d.preview != nil && !d.preview.done
	d.mu.Unlock()
	if checking || d.sl.checking.Load() {
		return "check"
	}
	return ""
}

// qrLeft stops the sign-in once the QR page is left, on Q2 Pod with "signin": the firmware keeps
// /tmp/q2-subsonic.qrpage while the page is open, so its going after it was seen is the page closed.
// A page never opened leaves it to the ten minutes.
func (d *daemon) qrLeft() {
	if !d.caps.Signin || !d.signin.active() {
		return
	}
	_, err := os.Stat(filepath.Join(d.tmp, "q2-subsonic.qrpage"))
	d.mu.Lock()
	left := err != nil && d.qrSeen
	d.qrSeen = err == nil
	d.mu.Unlock()
	if left {
		d.signin.stop()
	}
}

// idle is whether the app may exit: no request for idleAfter (the end of a sync counts as one), no
// sync, stream or Server Library check, no song that may still count as played or report on its
// way, and no library request awaiting its answer, as the firmware works on that only while the
// app runs (I15).
func (d *daemon) idle(now time.Time) bool {
	d.mu.Lock()
	last, lib := d.lastReq, d.lib != nil && !d.lib.done
	d.mu.Unlock()
	return now.Sub(last) >= idleAfter && !lib && !d.runner.Busy() && !d.stream.active() && !d.stream.plays.pending() &&
		!d.sl.busy.Load()
}

// close ends what the app runs before it exits; a sync paused here resumes at the next Sync.
func (d *daemon) close(sock string) {
	d.signin.stop()
	d.stream.stop()
	d.runner.Pause()
	d.art.remove()
	d.state.remove()
	os.Remove(sock)
}

// serve answers Q2SPOT/1 on sock, and exits 0 once idle.
func (d *daemon) serve(sock string) error {
	go q2.Every(time.Second, nil, func() {
		d.tick()
		if d.idle(time.Now()) {
			d.log.Printf("idle, exiting")
			d.close(sock)
			os.Exit(0)
		}
	})
	return q2spot.Serve(sock, d.device)
}
