package main

import (
	"bufio"
	"context"
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
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Lyrics from the server, asked for by the firmware (Q2 Pod's docs/internals.md#server-lyrics):
// when its lyrics page shows a song from the server that has no lyrics of its own (no .lrc beside
// it, no LYRICS tag), it writes /tmp/q2-subsonic.lrcask, "<seq> <card path>". The app finds the
// song's id (songOf), asks the server once, and writes what it gives beside the song as LRC
// (library.LrcFile), where the firmware's own loader finds it from then on: "[mm:ss.xx]" lines when
// synced, plain lines when not. The answer is the state file's lyricsack=<seq> <code>. A song the
// server has none for is remembered in .subsonic/lyrics-none.txt for lyricsNoneFor, and a server
// with no lyrics call at all for the app's run, so neither is asked again. Nothing is fetched
// ahead: one request for a song the firmware shows, and none for a song asked about before.

// Answers to a .lrcask request, in the state file's lyricsack=<seq> <code>.
const (
	lyricsWritten = 0 // the .lrc is beside the song: the firmware loads it
	lyricsNotOurs = 1 // not a server song of this app's
	lyricsNone    = 2 // the server has no lyrics for it
	lyricsFailed  = 3 // not asked or no answer: signed out, the server out of reach, the song gone
)

// lyricsNoneFor is how long a song the server had no lyrics for is not asked about again, and
// lyricsNoneMax how many such songs are kept, the oldest dropped first.
const (
	lyricsNoneFor = 30 * 24 * time.Hour
	lyricsNoneMax = 5000
)

// lyricsIn is the server's time to answer a request's calls.
var lyricsIn = 20 * time.Second

// The server's lyrics calls, learned once per server in the app's run.
const (
	lyrUnknown    = iota
	lyrSongLyrics // OpenSubsonic's getLyricsBySongId
	lyrPlain      // only Subsonic's getLyrics, by artist and title
	lyrNoCalls    // neither: every song is none
)

type lyricsState struct {
	run    sync.Mutex // one request at a time, its checks and writes included
	mu     sync.Mutex
	req    string           // the .lrcask last read, or there at start-up
	server string           // the server calls is for
	calls  int              // lyrUnknown…lyrNoCalls
	none   map[string]int64 // "<account> <id>": when the server had none for it (Unix seconds); nil until read
}

func (d *daemon) lyricsPath() string     { return filepath.Join(d.tmp, "q2-"+appID+".lrcask") }
func (d *daemon) lyricsNonePath() string { return filepath.Join(d.dir, "lyrics-none.txt") }

// lyricsInit takes a request left from before this start as answered, as radioInit does: the
// firmware writes its request only once the app's socket is there.
func (d *daemon) lyricsInit() {
	if b, err := os.ReadFile(d.lyricsPath()); err == nil {
		d.lyr.mu.Lock()
		d.lyr.req = string(b)
		d.lyr.mu.Unlock()
	}
}

// lyricsTick answers the firmware's .lrcask request, each once; the server is asked in the
// background.
func (d *daemon) lyricsTick() {
	b, err := os.ReadFile(d.lyricsPath())
	d.lyr.mu.Lock()
	if err != nil || string(b) == d.lyr.req {
		d.lyr.mu.Unlock()
		return
	}
	d.lyr.req = string(b)
	d.lyr.mu.Unlock()
	line := strings.TrimRight(strings.SplitN(string(b), "\n", 2)[0], "\r")
	n, path, _ := strings.Cut(line, " ")
	seq, err := strconv.ParseInt(n, 10, 64)
	if err != nil || path == "" {
		d.log.Printf("lyrics: bad request %q", line)
		return
	}
	d.spawn(func() {
		code := d.lyricsFor(path)
		d.state.setLyricsAck(fmt.Sprintf("%d %d", seq, code))
	})
}

// lyricsFor puts the server's lyrics for the song at path beside it, and says how that went.
func (d *daemon) lyricsFor(path string) int {
	d.lyr.run.Lock()
	defer d.lyr.run.Unlock()
	if filepath.Clean(path) != path || !strings.HasPrefix(path, d.card+"/") || !library.IsSong(path) {
		return lyricsNotOurs
	}
	id, _ := d.songOf(path)
	if id == "" {
		return lyricsNotOurs
	}
	lrc := library.LrcFile(path)
	if exists(lrc) { // written before; the firmware asks only when it found none, so this is a race
		return lyricsWritten
	}
	cl, acct := d.client(), d.acct()
	if cl == nil || acct == "" {
		return lyricsFailed
	}
	key := acct + " " + id
	if d.lyricsNoneHas(key) {
		return lyricsNone
	}
	ctx, cancel := context.WithTimeout(context.Background(), lyricsIn)
	defer cancel()
	l, ok, err := d.lyricsAsk(ctx, cl, id)
	if err != nil {
		d.log.Printf("lyrics: %s: %v", id, err)
		return lyricsFailed
	}
	if !ok {
		d.lyricsNoneAdd(key)
		return lyricsNone
	}
	if !exists(path) { // the song left the card while the server answered
		return lyricsFailed
	}
	if err := library.WriteOwned(d.card, lrc, []byte(lrcText(l))); err != nil {
		d.log.Printf("lyrics: %s: %v", id, err)
		return lyricsFailed
	}
	return lyricsWritten
}

// lyricsAsk is the server's lyrics for song id, by the calls it has: ok false when it has none, an
// error when it didn't answer.
func (d *daemon) lyricsAsk(ctx context.Context, cl *subsonic.Client, id string) (subsonic.Lyrics, bool, error) {
	calls, err := d.lyricsCalls(ctx, cl)
	if err != nil {
		return subsonic.Lyrics{}, false, err
	}
	switch calls {
	case lyrSongLyrics:
		l, ok, err := cl.LyricsBySongID(ctx, id)
		if errors.Is(err, subsonic.ErrNotFound) {
			return l, false, nil
		}
		return l, ok, err
	case lyrPlain:
		sg, err := cl.Song(ctx, id)
		if errors.Is(err, subsonic.ErrNotFound) {
			return subsonic.Lyrics{}, false, nil
		}
		if err != nil {
			return subsonic.Lyrics{}, false, err
		}
		text, err := cl.PlainLyrics(ctx, sg.Artist, sg.Title)
		if subsonic.Unsupported(err) { // the server has no lyrics call at all
			d.lyr.mu.Lock()
			d.lyr.calls = lyrNoCalls
			d.lyr.mu.Unlock()
			return subsonic.Lyrics{}, false, nil
		}
		if err != nil || strings.TrimSpace(text) == "" {
			return subsonic.Lyrics{}, false, err
		}
		var l subsonic.Lyrics
		for _, s := range strings.Split(text, "\n") {
			l.Lines = append(l.Lines, subsonic.LyricLine{Text: s})
		}
		return l, true, nil
	}
	return subsonic.Lyrics{}, false, nil
}

// lyricsCalls is which lyrics calls the signed-in server has, asked once per server in the run.
func (d *daemon) lyricsCalls(ctx context.Context, cl *subsonic.Client) (int, error) {
	server := d.acct() // the account: its server and user
	d.lyr.mu.Lock()
	if d.lyr.server != server {
		d.lyr.server, d.lyr.calls = server, lyrUnknown
	}
	calls := d.lyr.calls
	d.lyr.mu.Unlock()
	if calls != lyrUnknown {
		return calls, nil
	}
	ok, err := cl.Offers(ctx, "songLyrics")
	if err != nil {
		return 0, err
	}
	calls = lyrPlain
	if ok {
		calls = lyrSongLyrics
	}
	d.lyr.mu.Lock()
	if d.lyr.server == server {
		d.lyr.calls = calls
	}
	d.lyr.mu.Unlock()
	return calls, nil
}

// lrcText is l as LRC: "[mm:ss.xx]" before each line when synced, with the offset applied as LRC's
// own (a positive one shows every line sooner); a line a line when not. A line's own line breaks
// become spaces in a synced one, and lines in a plain one.
func lrcText(l subsonic.Lyrics) string {
	var b strings.Builder
	for _, ln := range l.Lines {
		text := strings.ReplaceAll(strings.TrimRight(ln.Text, "\r"), "\r\n", "\n")
		if !l.Synced {
			for _, s := range strings.Split(text, "\n") {
				b.WriteString(strings.TrimRight(s, "\r") + "\n")
			}
			continue
		}
		ms := max(ln.Start-l.Offset, 0)
		fmt.Fprintf(&b, "[%02d:%02d.%02d]%s\n", ms/60000, ms/1000%60, ms%1000/10, strings.ReplaceAll(text, "\n", " "))
	}
	return strings.Trim(b.String(), "\n") + "\n"
}

// lyricsNoneLoad reads lyrics-none.txt, "<account> <id> <unix seconds>" a line, once. d.lyr.mu is held.
func (d *daemon) lyricsNoneLoad() {
	if d.lyr.none != nil {
		return
	}
	d.lyr.none = map[string]int64{}
	f, err := os.Open(d.lyricsNonePath())
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		if at, err := strconv.ParseInt(f[2], 10, 64); err == nil {
			d.lyr.none[f[0]+" "+f[1]] = at
		}
	}
}

func (d *daemon) lyricsNoneHas(key string) bool {
	d.lyr.mu.Lock()
	defer d.lyr.mu.Unlock()
	d.lyricsNoneLoad()
	at, ok := d.lyr.none[key]
	return ok && time.Since(time.Unix(at, 0)) < lyricsNoneFor
}

// lyricsNoneAdd remembers key and writes the file again, without the songs past lyricsNoneFor and,
// over lyricsNoneMax, the oldest.
func (d *daemon) lyricsNoneAdd(key string) {
	d.lyr.mu.Lock()
	defer d.lyr.mu.Unlock()
	d.lyricsNoneLoad()
	now := time.Now()
	d.lyr.none[key] = now.Unix()
	type kept struct {
		key string
		at  int64
	}
	var all []kept
	for k, at := range d.lyr.none {
		if now.Sub(time.Unix(at, 0)) < lyricsNoneFor {
			all = append(all, kept{k, at})
		} else {
			delete(d.lyr.none, k)
		}
	}
	slices.SortFunc(all, func(a, b kept) int { return int(b.at - a.at) })
	for _, k := range all[min(len(all), lyricsNoneMax):] {
		delete(d.lyr.none, k.key)
	}
	all = all[:min(len(all), lyricsNoneMax)]
	var b strings.Builder
	for _, k := range all {
		fmt.Fprintf(&b, "%s %d\n", k.key, k.at)
	}
	if err := library.WriteOwned(d.card, d.lyricsNonePath(), []byte(b.String())); err != nil {
		d.log.Printf("lyrics: %v", err)
	}
}
