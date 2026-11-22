package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/syncer"
)

// streamWindow is the songs kept behind the one playing, and fetched ahead of it when the firmware
// sends no .ahead (Download Ahead, ahead.go).
const streamWindow = 2

// idleLooks is how many polls the run loop waits between looks while it has nothing to fetch.
const idleLooks = 5

type streamSource interface {
	PlayURL(id, quality string) string
	Get(ctx context.Context, u string, from int64) (*http.Response, error)
	Ping(ctx context.Context) error
}

// pingIn is how long the server has to answer the ping that tells a lost server from one bad song.
const pingIn = 10 * time.Second

// streamer plays a list by fetching the songs after .playing (plan: the firmware's .ahead, or a
// couple in list order) into the hidden stream folder and telling the firmware what to play through .play. Streamed files never go in the manifest.
type streamer struct {
	root, playFile, playingFile string // root is the card (/mnt/mmc)
	caps                        fw.Caps
	src                         streamSource
	manifest                    func() *library.Manifest
	net                         *syncer.NetSlot
	poll                        time.Duration
	backoff                     []time.Duration      // wait after each failed try of a song; one try per entry
	log                         *applog.Log          // optional
	quality                     func() string        // optional Streaming Quality; mp3-320/mp3-128 transcode every streamed song
	moving                      func(id string) bool // optional: a sync running or paused will move or remove this song
	// idle ends a stream when no new song has played for this long after the one playing should
	// have ended, and nothing is left to fetch: the firmware leaves .playing alone once it plays
	// anything else, so this is also how a stream learns that it was left. Default idleAfter.
	idle time.Duration
	// kick wakes the run loop at once: the daemon sends on it when .playing, .ahead or .retry change.
	kick   chan struct{}
	kicked atomic.Bool // a watch sends on kick (watchNames): the idle loop may wait idleLooks polls
	// removeHook, a test's, takes dropBehind's deletes in place of library.Remove.
	removeHook func(path string)
	// gap is the least time between two writes of a queue file: the firmware reads it again when
	// its mtime's second changes, and FAT32 keeps mtimes in 2 s.
	gap time.Duration
	// plays reports the songs .playing names to the server.
	plays *plays
	// played, optional, is told the id of each server song .playing names (a radio's recent songs).
	played func(id string)
	// started, optional, is told of each list started, and whether it is a radio's.
	started func(radio bool)
	// The stream cache (cache.go). cacheCap nil: none, and streamed files go as the list plays on.
	cacheCap func() int64            // its limit in bytes; 0 is Off
	acct     func() string           // the signed-in account's folder name, "" when signed out
	free     func(path string) int64 // the card's free bytes
	reserve  func() int64            // the bytes a sync running or paused still needs
	clock    func() time.Time        // the app's clock for a kept file's time; it may wait (now)
	cmu      sync.Mutex              // one walk at a time
	kmu      sync.Mutex              // keep, and each delete
	keep     map[string]bool         // the protected songs (protect)
	used     atomic.Int64            // bytes kept, as last walked and fetched since; for Settings, and when to walk
	counted  atomic.Bool             // a walk has set used

	// The Server Library's hooks, both optional: avail is told of each song the cache gains (kept)
	// or loses, and pinged of each ping that tells a lost server from one bad song, and its answer.
	avail  func(path string, kept bool)
	pinged func(err error)
	// gaveUp, optional, is told of each song given up (giveUp).
	gaveUp func(e *streamErr)

	errMu sync.Mutex
	err   *streamErr

	xfer atomic.Bool // a song of the window is still to fetch (busy)

	// Download Ahead (ahead.go): the firmware's .ahead and the app's .stream, both "" without the
	// files; .stream's pace while a song arrives, and when one with no byte reads stall.
	aheadFile, statusFile string
	// retryFile is Now Playing's Retry (/tmp/q2-<id>.retry): a song waiting out its backoff or a
	// lost server is tried again at once. "" without it.
	retryFile               string
	statusEvery, stallAfter time.Duration
	gmu                     sync.Mutex
	growing                 map[string]bool // songs arriving in place ("grow"), as .growing lists them
	pmu                     sync.Mutex      // the run's progress, and .stream's writes

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func newStreamer(root, play, playing string, caps fw.Caps, src streamSource, man func() *library.Manifest,
	net *syncer.NetSlot, poll time.Duration) *streamer {
	return &streamer{root: root, playFile: play, playingFile: playing, caps: caps, src: src, manifest: man, net: net, poll: poll,
		backoff: []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second},
		gap:     2100 * time.Millisecond, // 2 s and a tick of the kernel's coarse clock
		plays:   &plays{unit: time.Second}, kick: make(chan struct{}, 1)}
}

// streamErr is a song that failed all its tries: what the root shows, and its Details.
type streamErr struct {
	title, id string
	at        time.Time
	err       error // the last try's
	skipped   bool  // dropped from the queue, and the list played on
}

func (e *streamErr) Error() string {
	if e.skipped {
		return "Skipped " + e.title + ": " + subsonic.Message(e.err, "")
	}
	return "Couldn't stream " + e.title
}
func (e *streamErr) Unwrap() error { return e.err }

// Err is set when a song failed all its tries; prefetching stops at that song, unless it was
// skipped. A new list or a stop clears it.
func (s *streamer) Err() *streamErr {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// giveUp records a song the run gave up on, skipped or stopped at, and tells gaveUp of it (the
// daemon's server-error toast, reach.go).
func (s *streamer) giveUp(e *streamErr) {
	s.setErr(e)
	if s.gaveUp != nil {
		s.gaveUp(e)
	}
}

func (s *streamer) setErr(err *streamErr) {
	s.errMu.Lock()
	s.err = err
	s.errMu.Unlock()
}

func (s *streamer) logf(format string, a ...any) {
	if s.log != nil {
		s.log.Printf("%s", applog.Redact(fmt.Sprintf(format, a...)))
	}
}

func (s *streamer) dir() string { return filepath.Join(s.root, ".subsonic", "stream") }

func (s *streamer) active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// busy is whether the stream moves data: a song of the window is still to fetch. Waiting for the
// firmware to play on is not busy, so the Q2 may sleep while a list is paused (I14).
func (s *streamer) busy() bool { return s.active() && s.xfer.Load() }

func (s *streamer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	s.plays.clear() // no play goes to another account
	s.setErr(nil)
	if s.statusFile != "" {
		os.Remove(s.statusFile) // nothing streams: the firmware shows no song loading
	}
}

func (s *streamer) stopLocked() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
		s.cancel = nil
	}
	s.plays.cancel() // the song playing is left; one that counts after its run ended, too
}

// playingNow is .playing, and when the firmware wrote it. Each write is a new file renamed into
// place (blob_io), made only when the song changes or a new list starts: so a new time is a
// song's start, also at the path the file had.
func (s *streamer) playingNow() (string, time.Time) {
	f, err := os.Open(s.playingFile)
	if err != nil {
		return "", time.Time{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", time.Time{}
	}
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	return strings.TrimSpace(string(b)), st.ModTime()
}

// clean empties the stream folder of its files, but for the spare ones (a radio's first song, which
// plays as it is): a new list, or a daemon start, leaves no uncached songs or queue from an older
// one on the card (M26). The cache's folders stay.
func (s *streamer) clean(spare ...string) {
	s.dropGrowing()
	ents, _ := os.ReadDir(s.dir())
	for _, e := range ents {
		if !e.IsDir() && !slices.Contains(spare, filepath.Join(s.dir(), e.Name())) {
			library.Remove(s.root, filepath.Join(s.dir(), e.Name()))
		}
	}
}

// cleanLeft is clean at a daemon start, but for a stream the firmware may still play: .playing
// names a song that is there, of the folder or of the newest queue file. The app exits when idle
// with the queue still playing, and Go to starts it again from Now Playing. Such a folder goes at
// the next Play, or at a start after the Q2's own restart, which empties /tmp. The queue's songs
// from the one playing on are protected again, as its run had them; only the songs around it when
// it started so long ago that the queue was left, or is paused, as a run's end has it.
// ponytail: the songs' lengths aren't known here, so an hour stands for the one playing; read the
// length from the file if a longer one that still plays loses the songs after its window to a clear.
func (s *streamer) cleanLeft() {
	s.dropGrowing()
	s.protect(nil)
	if p, at := s.playingNow(); p != "" && exists(p) {
		q := s.lastQueue()
		i := slices.Index(q, p)
		if i >= 0 {
			hi := len(q)
			if time.Since(at) >= cmp.Or(s.idle, idleAfter)+time.Hour {
				hi = min(i+streamWindow+1, hi)
			}
			s.protect(q[max(i-streamWindow, 0):hi])
		}
		if i >= 0 || filepath.Dir(p) == s.dir() {
			return
		}
	}
	s.clean()
}

// job is one song of the list: where it plays from, and how to fetch it when it is not synced.
type job struct {
	path    string
	fetch   bool
	cached  bool // kept in the stream cache: never deleted behind the song playing
	quality string
	fails   int       // failed tries
	next    time.Time // no try before
	landed  bool      // fetched once in this run: the firmware may have passed it
	away    bool      // the server was lost at it: tried on until it answers or the list is left
	failed  bool      // it ended the run (.stream's fail)
	partial bool      // in place (grow), cut mid-song: its bytes kept for the player, the next try from their end
}

// synced reports whether e's file can play from the card: it is there, and no sync is about to
// move or remove it (M24). (The firmware passes over only a real ".." segment, which a Clean path
// has none of, so an ellipsis in a name plays: I19.)
func (s *streamer) synced(e *library.Entry) bool {
	return e != nil && exists(e.Path) && (s.moving == nil || !s.moving(e.ID))
}

// streamQuality is Streaming Quality as set now, "" for none: read from the card's settings.json,
// so once per list start (or radio refill), not once a song (a 9,199-song Shuffle read it 9,199
// times before its first request).
func (s *streamer) streamQuality() string {
	if s.quality == nil {
		return ""
	}
	return s.quality()
}

// format is the extension and quality a song not on the card plays at, with Streaming Quality sq.
func (s *streamer) format(sg subsonic.Song, sq string) (ext, q string) {
	ext, q = strings.ToLower(sg.Suffix), "original"
	if s.caps.Playlist && !library.Playable(ext) || !s.caps.Playlist && ext != "flac" && ext != "mp3" {
		ext, q = "mp3", "mp3-320"
	}
	if sq == "mp3-320" || sq == "mp3-128" {
		ext, q = "mp3", sq
	}
	return ext, q
}

// streamID is the song id in a streamed file's name, "NNNN - <title> [<id>].<ext>" as start writes
// it, or the cache's "<title> [<id>].<ext>"; "" for any other name.
// ponytail: an id SafeName changed (FAT-reserved characters, over 120 bytes) or one holding " ["
// does not come back; keep path→id in the streamer if a server's ids need it.
func streamID(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	i := strings.LastIndex(name, " [")
	if i < 0 || !strings.HasSuffix(name, "]") {
		return ""
	}
	return name[i+2 : len(name)-1]
}

// start replaces whatever streams with songs, playing from index from.
func (s *streamer) start(songs []subsonic.Song, from int) { s.startRun(songs, nil, from, nil, nil) }

// startRadio is start for a radio (radio.go): paths[i], when not "", is a card file that plays as it
// is, and refill is asked for more songs whenever radioLow or fewer are left after the one playing.
func (s *streamer) startRadio(songs []subsonic.Song, paths []string, refill func() []subsonic.Song) {
	s.startRun(songs, paths, 0, nil, refill)
}

// catPlay is a queue the firmware asked for from its Library (.catplay): seq answers it in .play,
// and local is the stream cache's copies by song id, of any quality, which play as they are.
type catPlay struct {
	seq   int64
	local map[string]string
}

// startCat is start for a catalogue queue: paths[i], when not "", is a card file that plays as it
// is (the user's own, or a synced song) in place of songs[i]. While the server is lost, the queue
// waits at the song it can't fetch, as any queue does, and Now Playing says why with Retry and
// Skip (the user's ruling, 2026-11-19: no silent skip).
func (s *streamer) startCat(songs []subsonic.Song, paths []string, from int, c *catPlay) {
	s.startRun(songs, paths, from, c, nil)
}

func (s *streamer) startRun(songs []subsonic.Song, paths []string, from int, cat *catPlay, refill func() []subsonic.Song) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	if from < 0 || from >= len(songs) {
		return
	}
	s.setErr(nil)
	if s.started != nil {
		s.started(refill != nil)
	}
	stale, staleAt := s.playingNow() // belongs to the previous list
	s.clean(paths...)                // old queue file, stale and half-fetched songs; not a card file that plays as it is
	m := s.manifest()
	jobs := make([]job, len(songs))
	base, sq := s.cacheBase(), s.streamQuality()
	for i, sg := range songs {
		if i < len(paths) && paths[i] != "" {
			jobs[i] = job{path: paths[i]}
			continue
		}
		jobs[i] = s.jobFor(i, sg, m, base, sq, cat)
	}
	seq := time.Now().UnixNano() // never reused
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	r := &streamRun{s: s, ctx: ctx, songs: slices.Clone(songs), jobs: jobs, from: from, last: from, seq: seq,
		stale: stale, staleAt: staleAt, heard: staleAt, playSeq: seq, refill: refill, prog: progress{job: -1}}
	if cat != nil {
		r.cat, r.playSeq = true, cat.seq
	}
	r.protect(from, len(jobs))
	if s.caps.Playlist {
		r.queue = filepath.Join(s.dir(), fmt.Sprintf("queue-%d.m3u", seq))
		if err := r.writeQueue(); err != nil {
			cancel()
			close(s.done)
			return
		}
		r.announce(r.queue)
	}
	go func() { defer close(s.done); r.run() }()
}

type streamRun struct {
	s         *streamer
	ctx       context.Context
	songs     []subsonic.Song
	jobs      []job
	from      int
	quietBy   time.Time // when no song has started for the idle time: a lost server is waited for until then
	last      int       // the index .playing named last: a song in the list twice is found after it (M25)
	dropFrom  int       // dropBehind's songs before it are deleted already
	queueTo   int       // the queue file names the list's songs up to it (queueAhead)
	wholeCur  int       // plan's: for cur wholeCur, the songs from cur+1 to wholeTo were whole at wholeAt
	wholeTo   int
	wholeAt   time.Time
	seq       int64
	announced bool
	stale     string    // .playing at start: ignored until it is written again
	staleAt   time.Time // when it was written
	heard     time.Time // .playing's time when a song's start was last told to plays
	queue     string    // the playlist's file; "" in folder mode
	wrote     time.Time // when it was last written
	skips     int       // songs skipped since the last one fetched
	keptLo    int       // the first job protected, and how many jobs there were then
	keptN     int
	cat       bool                   // a catalogue queue (startCat)
	playSeq   int64                  // the seq .play names: seq, or the .catplay's it answers
	refill    func() []subsonic.Song // a radio's: more songs for its end (startRadio); nil for any other list
	refillAt  time.Time              // no ask for more before, after one that brought none
	prog      progress               // the download running now, for .stream (s.pmu)
}

// jobFor is how song sg, the list's i-th, plays: from the card when it is synced, from the stream
// cache's copy (cat's for a catalogue queue, or one to fetch there), else fetched into the stream folder.
func (s *streamer) jobFor(i int, sg subsonic.Song, m *library.Manifest, base, sq string, cat *catPlay) job {
	if e := m.Songs[sg.ID]; s.caps.Playlist && s.synced(e) {
		return job{path: e.Path}
	}
	if p := cat.copy(sg.ID); p != "" {
		return job{path: p, cached: true}
	}
	ext, q := s.format(sg, sq)
	if p := s.cachePath(base, sg, ext, q); p != "" {
		return job{path: p, fetch: true, quality: q, cached: true}
	}
	name := fmt.Sprintf("%04d - %s [%s].%s", i, library.SafeName(sg.Title), library.SafeName(sg.ID), ext)
	return job{path: filepath.Join(s.dir(), name), fetch: true, quality: q}
}

// radioLow is how many songs a radio has left after the one playing when it asks for more, and
// radioRetry how long it waits to ask again after an ask that brought none.
const (
	radioLow   = 10
	radioRetry = 30 * time.Second
)

// more asks a radio for songs once radioLow or fewer are left after cur, and adds them to the end of
// the list and the queue file, which the firmware reads again and plays on into. 1 when it added any.
func (r *streamRun) more(cur int) bool {
	if r.refill == nil || r.queue == "" || len(r.jobs)-1-cur > radioLow || time.Now().Before(r.refillAt) {
		return false
	}
	add := r.refill()
	if r.ctx.Err() != nil {
		return false
	}
	if len(add) == 0 {
		r.refillAt = time.Now().Add(radioRetry)
		return false
	}
	m, base, sq := r.s.manifest(), r.s.cacheBase(), r.s.streamQuality()
	for _, sg := range add {
		r.jobs = append(r.jobs, r.s.jobFor(len(r.jobs), sg, m, base, sq, nil))
		r.songs = append(r.songs, sg)
	}
	select { // the firmware reads the file again only when its mtime's second changes
	case <-r.ctx.Done():
		return false
	case <-time.After(r.s.gap - time.Since(r.wrote)):
	}
	if err := r.writeQueue(); err != nil {
		r.s.logf("stream: radio: queue: %v", err)
		return false
	}
	r.s.logf("stream: radio: %d songs added, %d in the list", len(add), len(r.jobs))
	r.protect(max(r.from, cur-streamWindow), len(r.jobs))
	return true
}

// copy is the stream cache's whole copy of song id, "" when it has none (or c is nil).
func (c *catPlay) copy(id string) string {
	if c == nil || c.local[id] == "" || !exists(c.local[id]) {
		return ""
	}
	return c.local[id]
}

// protect holds the list's kept songs from index lo to hi, the last one playable, in the cache.
func (r *streamRun) protect(lo, hi int) {
	var paths []string
	for _, j := range r.jobs[lo:min(hi, len(r.jobs))] {
		if j.cached {
			paths = append(paths, j.path)
		}
	}
	r.keptLo, r.keptN = lo, len(r.jobs)
	r.s.protect(paths)
}

// writeQueue writes the queue file whole, a line a song from the first played on, aside and
// renamed into place. The firmware waits at a song not there yet and has queued the lines before
// it, so only that song and the ones after it may change.
// queueAhead is how far past the song playing the queue file names the list: the firmware reads the
// file to 256 KB (addon.c M3U_MAX), about 2,300 cache paths, and a 9,199-song Shuffle's are about
// 1 MB, so the songs past it would never queue. The file is extended once the song playing comes
// within half of it of the end (run), its lines before then the same, as the firmware needs.
const queueAhead = 1000

func (r *streamRun) writeQueue() error {
	end := min(len(r.jobs), max(r.queueTo, max(r.last, r.from)+queueAhead+1))
	var paths []string
	for _, j := range r.jobs[r.from:end] {
		paths = append(paths, j.path)
	}
	if err := library.WriteM3U(r.s.root, r.queue, paths); err != nil {
		return err
	}
	r.wrote, r.queueTo = time.Now(), end
	return nil
}

// gone reports the server's own word that a song isn't there (Subsonic 70, HTTP 410), which no
// retry changes. A bare 404 is retried: a proxy or a deploy can answer one for a while.
func gone(err error) bool {
	var ae *subsonic.APIError
	return errors.As(err, &ae) && (ae.Code == 70 || ae.HTTP == 410)
}

// lost reports whether err is the server out of reach, which may pass. A card that can't be
// written reads as a net.Error too (an errno is one), and waiting mends none of those.
func lost(err error) bool {
	var pe *fs.PathError
	var le *os.LinkError
	return errors.Is(syncer.StopReason(err), syncer.ErrOffline) && !errors.As(err, &pe) && !errors.As(err, &le)
}

// serverLost asks the server itself with a ping: one song's own stall, cut body or proxy error reads
// as a lost server too, and waiting mends none of those. An answer of any kind is a server there.
func (r *streamRun) serverLost() bool {
	ctx, cancel := context.WithTimeout(r.ctx, pingIn)
	defer cancel()
	err := r.s.src.Ping(ctx)
	if r.s.pinged != nil && r.ctx.Err() == nil {
		r.s.pinged(err)
	}
	return serverAway(err)
}

// serverAway is a lost server, or a proxy answering for one that isn't there (502, 503, 504: reachCode):
// waited for, never skipped past (the user's ruling, 2026-11-19: no silent skip; Now Playing says
// why and offers Retry and Skip).
func serverAway(err error) bool { return lost(err) || reachCode(err) != "" }

// failed counts a failed try of song i and reports whether the run goes on, and whether the song
// was skipped. A song is tried once a backoff entry (in a playlist, once when it is gone). Then,
// in a playlist, a song the server can't send (gone, a transcode that fails, an error for a body)
// is dropped from the list and the queue file, and the firmware plays on past it. A lost server
// is waited for. A song that fails as a lost server would while the server answers a ping, a
// refusal for the account, a card that can't be written (StopReason: an errno is a net.Error), a
// third song in a row, or a song that landed before (the firmware may have passed its line, which
// must stay) ends the run at it, the queue waiting there.
func (r *streamRun) failed(i int, err error) (goOn, skipped bool) {
	j, sg, s := &r.jobs[i], r.songs[i], r.s
	j.fails++
	s.logf("stream %q try %d: %v", sg.Title, j.fails, err)
	if j.fails < len(s.backoff) && !(r.queue != "" && gone(err)) {
		j.next = time.Now().Add(s.backoff[j.fails-1])
		return true, false
	}
	e := &streamErr{title: sg.Title, id: sg.ID, at: time.Now(), err: err}
	// The server is lost (Wi-Fi dropped, the Q2 slept while paused): the songs already fetched
	// play on, so the song is tried on at the backoff's last pace until the server answers. It
	// ends only when no song has started for the idle time: the list was left. A server that
	// answers the ping is not lost: the failure is the song's own, for the rules below.
	if serverAway(err) && time.Now().Before(r.quietBy) {
		if r.serverLost() {
			j.away, j.next = true, time.Now().Add(s.backoff[len(s.backoff)-1])
			s.setErr(e)
			return true, false
		}
		// The server came back while this try was failing: the try was the outage's, not the
		// song's. It starts over with its tries, and a song that is bad too ends as any other.
		if j.away {
			j.away, j.fails, j.next = false, 0, time.Time{}
			s.setErr(nil)
			return true, false
		}
	}
	if r.queue == "" || j.landed || syncer.StopReason(err) != nil || r.skips >= 2 {
		s.giveUp(e)
		return false, false
	}
	select {
	case <-r.ctx.Done():
		return false, false
	case <-time.After(s.gap - time.Since(r.wrote)):
	}
	r.jobs, r.songs = slices.Delete(r.jobs, i, i+1), slices.Delete(r.songs, i, i+1)
	if werr := r.writeQueue(); werr != nil {
		s.logf("stream: queue: %v", werr)
		s.giveUp(e)
		return false, false
	}
	r.skips++
	e.skipped = true
	s.giveUp(e)
	s.logf("stream: skipped %s (%q): %v", sg.ID, sg.Title, err)
	r.last = min(r.last, len(r.jobs)-1)
	return r.from < len(r.jobs), true // nothing left to play ends the run
}

func (r *streamRun) announce(path string) {
	if err := q2.WriteFile(r.s.playFile, []byte(fmt.Sprintf("%d %s", r.playSeq, path))); err == nil {
		r.announced = true
	}
}

// current is the index of the song .playing names, looked for from the last one found on, then back
// from it (a song in the list twice is the occurrence nearest), else the last one; and .playing as
// read, with its time.
func (r *streamRun) current() (int, string, time.Time) {
	p, at := r.s.playingNow()
	if p == "" || p == r.stale && at.Equal(r.staleAt) {
		return r.last, p, at
	}
	for i := r.last; i < len(r.jobs); i++ {
		if r.jobs[i].path == p {
			r.last = i
			return i, p, at
		}
	}
	for i := r.last - 1; i >= r.from; i-- {
		if r.jobs[i].path == p {
			r.last = i
			return i, p, at
		}
	}
	return r.last, p, at
}

func (r *streamRun) run() {
	defer r.s.xfer.Store(false)
	idle := r.s.idle
	if idle == 0 {
		idle = idleAfter
	}
	seen, since := "", time.Now() // .playing as read last, and since when
	read := time.Now()            // when .playing was read last
	for r.ctx.Err() == nil {
		r.takeRetry()
		cur, now, at := r.current()
		// A start seen now is no older than the last read, whatever the file's time says: the
		// clock may have been set meanwhile.
		late := min(time.Since(at), time.Since(read))
		read = time.Now()
		if now != seen {
			seen, since = now, time.Now()
		}
		// A song of this list started: .playing was written, and names it. Not "now != seen": a
		// list started again at the path .playing had is a start too. A fetch may have kept this
		// loop from seeing it for a while, which the file's time tells.
		if !at.Equal(r.heard) {
			if r.heard = at; now == r.jobs[cur].path && r.songs[cur].ID != "" { // a card file of a catalogue queue is not the server's
				r.s.plays.now(r.songs[cur], late)
				if r.s.played != nil {
					r.s.played(r.songs[cur].ID)
				}
				if r.jobs[cur].cached { // played now: the last to be evicted
					library.Touch(r.s.root, now, r.s.now())
				}
			}
		}
		// Whether .playing names cur for this list: cur is also where it starts, before the
		// firmware has played anything of it.
		named := now == r.jobs[cur].path && !(now == r.stale && at.Equal(r.staleAt))
		plan := r.plan(cur)
		r.dropBehind(cur, plan)
		if r.queue != "" && r.queueTo < len(r.jobs) && cur+queueAhead/2 >= r.queueTo && time.Since(r.wrote) >= r.s.gap {
			if err := r.writeQueue(); err != nil { // the list's next songs, for the firmware to queue
				r.s.logf("stream: queue: %v", err)
			}
		}
		if lo := max(r.from, cur-streamWindow); lo != r.keptLo || len(r.jobs) != r.keptN { // the song changed, or one was skipped
			r.protect(lo, len(r.jobs))
		}
		want := -1 // the first song of the plan still to fetch
		for _, i := range plan {
			if j := r.jobs[i]; j.fetch && !r.s.whole(j.path) {
				want = i
				break
			}
		}
		r.status(plan, -1)
		// Waiting for a lost server is no transfer: it must not keep the Q2 from standby.
		r.s.xfer.Store(want >= 0 && !r.jobs[want].away)
		r.quietBy = since.Add(idle + time.Duration(r.songs[cur].Duration)*time.Second)
		fetched := r.more(cur)
		if fetched {
			continue // the window and the run's end are looked at again with the new songs
		}
		if want >= 0 && !time.Now().Before(r.jobs[want].next) { // else backing off
			if err, replanned := r.fetchWatched(want); replanned {
				continue // the song left the plan while it arrived: its file is gone, plan again
			} else if err == nil {
				if r.jobs[want].away { // the server is back
					r.jobs[want].away = false
					r.s.setErr(nil)
				}
				fetched, r.skips, r.jobs[want].landed = true, 0, true
			} else if r.ctx.Err() != nil {
				return
			} else if goOn, skipped := r.failed(want, err); !goOn {
				if want < len(r.jobs) {
					r.jobs[want].failed = true
					r.status(plan, -1)
				}
				return
			} else if skipped {
				continue // the list changed under cur; and the firmware waits for the next song now
			}
		}
		if !r.announced && exists(r.jobs[r.from].path) {
			r.announce(r.s.dir()) // stock firmware: plays the folder once it has a song
		}
		// The last song plays, or none has started for the idle time after the one playing should
		// have ended (paused, or the firmware plays something else): nothing more to fetch. The songs
		// around it stay, as the firmware has them queued; those behind went above, and the rest go
		// at the next Play or start-up (M26).
		long := idle + time.Duration(r.songs[cur].Duration)*time.Second
		// A list left or paused no longer holds its songs to come in the cache (.playing can't tell
		// the two apart): a clear, a sign-out or a sync may take them.
		away := want >= 0 && r.jobs[want].away // a lost server ends the run in failed, once it is quiet
		if (want < 0 || away) && time.Since(since) >= long || want < 0 && named && cur == len(r.jobs)-1 {
			r.protect(max(r.from, cur-streamWindow), cur+streamWindow+1)
			return
		}
		if !fetched {
			// With nothing to fetch, and the daemon watching .playing, .ahead and .retry
			// (watchNames: a change wakes the loop at once), it looks again idleLooks polls on: a
			// paused list ran it once a second for the half hour before it ended.
			wait := r.s.poll
			if want < 0 && r.s.kicked.Load() {
				wait *= idleLooks
			}
			select {
			case <-r.ctx.Done():
			case <-r.s.kick:
			case <-time.After(wait):
			}
		}
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// dropBehind deletes the songs well behind the one playing, cur, but not one the plan has to come
// (Repeat All's next pass, named in .ahead): deleted, it was fetched again at once, and deleted
// again. Each song once: the pass is once a second, and a long list's songs passed, every one
// deleted again each pass, were tens of syscalls apiece. A song kept for the plan, or one fetched
// again since (a jump back), is looked at again when cur moves.
func (r *streamRun) dropBehind(cur int, plan []int) {
	if cur < r.dropFrom {
		r.dropFrom = r.from // a jump back: what is behind cur now is looked at afresh
	}
	lo, kept := max(r.dropFrom, r.from), -1
	for i := lo; i < cur-streamWindow; i++ {
		if !r.jobs[i].fetch || r.jobs[i].cached {
			continue
		}
		if slices.Contains(plan, i) {
			if kept < 0 {
				kept = i
			}
			continue
		}
		r.s.remove(r.jobs[i].path)
	}
	if hi := cur - streamWindow; hi > r.dropFrom {
		r.dropFrom = hi
		if kept >= 0 {
			r.dropFrom = kept
		}
	}
}

// remove deletes a streamed song from the card (library.Remove), or what a test put in its place.
func (s *streamer) remove(path string) {
	if s.removeHook != nil {
		s.removeHook(path)
		return
	}
	library.Remove(s.root, path)
}

// fetchWatched fetches job i while a watcher reads the plan again and writes .stream at its pace:
// a song that leaves the plan meanwhile (another song playing, a new .ahead) is cancelled, and
// replanned says so, its file already deleted by fetch.
func (r *streamRun) fetchWatched(i int) (err error, replanned bool) {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	r.s.pmu.Lock()
	kick := make(chan struct{}, 1)
	r.prog = progress{job: i, movedAt: time.Now(), lastBody: r.prog.lastBody, lastAt: r.prog.lastAt, lastArriving: r.prog.lastArriving,
		kick: kick}
	r.s.pmu.Unlock()
	left := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(min(r.s.poll, cmpOr(r.s.statusEvery, statusEvery)))
		defer tick.Stop()
		// .stream at once, its get line there before the first byte, then at each tick, and when the
		// bytes reach the firmware's start (startGate) or the first comes: the firmware gates a
		// song's start on the line, which a tick's wait left up to statusEvery old.
		for first := true; ; first = false {
			if !first {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				case <-kick:
				}
			}
			cur, _, _ := r.current()
			plan := r.plan(cur)
			if !slices.Contains(plan, i) {
				close(left)
				cancel()
				return
			}
			r.status(plan, i)
		}
	}()
	err = r.fetch(ctx, r.songs[i], r.jobs[i])
	if i < len(r.jobs) {
		r.jobs[i].partial = err != nil && r.s.grows() && exists(r.jobs[i].path) && !r.s.whole(r.jobs[i].path)
	}
	cancel()
	<-done
	r.s.pmu.Lock()
	r.prog.job = -1
	r.s.pmu.Unlock()
	select {
	case <-left:
		if r.ctx.Err() == nil && err != nil {
			r.s.logf("stream: %q left the plan while it arrived", r.songs[i].Title)
			return err, true
		}
	default:
	}
	return err, false
}

// fetch downloads one song, holding the network slot with priority so it goes before the
// syncer's next file: to <final>.part, synced and renamed, or with "grow" in place, listed in
// .growing until it is whole. Originals come through PlayURL, which servers allow when downloads
// are turned off (M27, R37).
func (r *streamRun) fetch(ctx context.Context, sg subsonic.Song, j job) (err error) {
	s := r.s
	trimmed := time.Now()
	if s.cacheCap != nil {
		s.trim(ctx, expect(sg, j.quality), s.reserve())
	}
	trimTook := time.Since(trimmed) // a walk of the card when the cache is near its limit
	if err := s.net.Acquire(ctx, true); err != nil {
		return err
	}
	defer s.net.Release()
	part, grow := j.path+".part", s.grows()
	if grow {
		part = j.path
	}
	// A song cut mid-way in place goes on from the end of its bytes, which the player may be playing
	// (a Range request; a server that sends it whole instead starts it over).
	from := int64(0)
	if st, err := os.Stat(part); grow && j.partial && err == nil {
		from = st.Size()
	}
	s.pmu.Lock()
	r.prog.began, r.prog.trim, r.prog.have = time.Now(), trimTook, from
	s.pmu.Unlock()
	tr := &getTrace{}
	resp, err := s.src.Get(httptrace.WithClientTrace(ctx, tr.trace()), s.src.PlayURL(sg.ID, j.quality), from)
	if err != nil {
		return err
	}
	s.logf("stream: %q: %s", sg.Title, tr.line(time.Now()))
	if from > 0 && resp.StatusCode != 206 {
		from = 0
		s.pmu.Lock()
		r.prog.have = 0
		s.pmu.Unlock()
	}
	s.pmu.Lock()
	r.prog.headers, r.prog.movedAt = time.Now(), time.Now()
	total := resp.ContentLength // as status writes it in the song's line
	if total <= 0 {
		total = expect(sg, j.quality)
	}
	r.prog.gate = startGate(total, int64(sg.Duration)*1000)
	s.pmu.Unlock()
	defer func() { r.fetchSummary(sg.Title, err) }()
	defer resp.Body.Close()
	// A cancel ends a read that waits: net/http's body does so itself, and any other is closed.
	stopRead := context.AfterFunc(ctx, func() { resp.Body.Close() })
	defer stopRead()
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		return err
	}
	keep := false // a cut song's bytes, kept in place and still listed in .growing
	if grow {
		if err := s.setGrowing(j.path, true); err != nil {
			return err
		}
		defer func() {
			if !keep {
				s.setGrowing(j.path, false) // after the file is whole, or deleted below
			}
		}()
	}
	var f *os.File
	if from > 0 {
		f, err = os.OpenFile(part, os.O_WRONLY|os.O_APPEND, 0o644)
	} else {
		f, err = os.Create(part)
	}
	if err != nil {
		return err
	}
	limit := syncer.Limit(resp.ContentLength, expect(sg, j.quality))
	s.pmu.Lock()
	r.prog.total = max(resp.ContentLength+from, 0)
	s.pmu.Unlock()
	n, err := io.Copy(io.MultiWriter(f, counter{r}), io.LimitReader(resp.Body, limit+1))
	if err == nil {
		err = syncer.SizeErr(n, resp.ContentLength, limit)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && !grow {
		err = os.Rename(part, j.path)
	}
	if why := syncer.StopReason(err); err != nil && grow && ctx.Err() == nil && n+from > 0 &&
		!errors.Is(why, syncer.ErrCardFull) && !errors.Is(why, syncer.ErrRefused) {
		keep = true // the connection lost mid-song: the player reads on, the next try from here
		r.s.logf("stream: %q: cut at %d bytes; kept, the next try from there", sg.Title, n+from)
	} else if err != nil {
		library.Remove(s.root, part)
	} else if j.cached {
		s.used.Add(n + from)
		library.Touch(s.root, j.path, s.now()) // the card's clock may be unset; the app's is the server's
		if s.avail != nil {
			s.avail(j.path, true)
		}
	}
	return err
}

// expect is about the bytes a song takes at quality: its server size, or for a transcode its
// duration at the bitrate when that is more; 0 when neither is known.
func expect(sg subsonic.Song, quality string) int64 {
	switch quality {
	case "mp3-320":
		return max(sg.Size, int64(sg.Duration)*320000/8)
	case "mp3-128":
		return max(sg.Size, int64(sg.Duration)*128000/8)
	}
	return sg.Size
}

// takeRetry takes Now Playing's Retry when the firmware has written it: every song still to fetch
// is due now, its backoff or a lost server's wait cut short. The file goes, so one press is one try.
func (r *streamRun) takeRetry() {
	if r.s.retryFile == "" || os.Remove(r.s.retryFile) != nil {
		return
	}
	r.s.logf("stream: retry asked")
	for k := range r.jobs {
		r.jobs[k].next = time.Time{}
	}
}
