package main

// Download Ahead and the streaming state (q2-coda's docs/server-library.md, "Download Ahead and the
// streaming state"). With the firmware's "ahead" word it writes /tmp/q2-<id>.ahead, the songs
// after the one playing in its own play order and how many to fetch, and reads
// /tmp/q2-<id>.stream, which this file writes: how far each planned song has come. With "grow"
// its player plays a song while it arrives, so a song is written in place and listed in
// stream/.growing until it is whole.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

const (
	statusEvery = 500 * time.Millisecond // .stream's pace while a song arrives: the firmware's 3 s staleness has room
	stallAfter  = 2 * time.Second        // no byte for this long: a get line reads stall
)

// aheadReq is the firmware's .ahead: for the .play of seq, fetch n of paths, the songs after the
// one playing in its play order.
type aheadReq struct {
	seq   int64
	n     int
	paths []string
}

// readAhead reads an .ahead file; ok is false when there is none or its header is not one.
func readAhead(path string) (a aheadReq, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return a, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return a, false
	}
	head := strings.Split(strings.TrimRight(sc.Text(), "\r"), "\t")
	if head[0] != "#Q2AHEAD/1" {
		return a, false
	}
	seen := 0
	for _, kv := range head[1:] {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "seq":
			if a.seq, err = strconv.ParseInt(v, 10, 64); err == nil {
				seen++
			}
		case "n":
			if a.n, err = strconv.Atoi(v); err == nil && a.n >= 0 {
				seen++
			}
		}
	}
	for sc.Scan() {
		if p := strings.TrimRight(sc.Text(), "\r"); p != "" {
			a.paths = append(a.paths, p)
		}
	}
	return a, seen == 2
}

// grows reports whether songs are written in place for a player that plays them as they arrive.
func (s *streamer) grows() bool { return s.caps.Playlist && s.caps.Ahead && s.caps.Grow }

// whole reports whether a song's file is there and complete: not one still arriving in place.
func (s *streamer) whole(path string) bool {
	s.gmu.Lock()
	g := s.growing[path]
	s.gmu.Unlock()
	return !g && exists(path)
}

// awaitSeed waits for a radio's first song to be whole when the list playing is still writing it: in
// place at path (a card file it was playing from), or into the cache at the path jobFor would give
// it. Starting the radio cancels that list's download, so it would be fetched twice, and a file
// arriving in place would be deleted under the player. It returns the path the radio plays the song
// from: path, or "" when the download failed and the radio fetches the song itself. At most radioIn.
func (s *streamer) awaitSeed(sg subsonic.Song, path string) string {
	ext, q := s.format(sg)
	cached := s.cachePath(s.cacheBase(), sg, ext, q)
	arriving := func(p string) bool {
		s.gmu.Lock()
		defer s.gmu.Unlock()
		return p != "" && (s.growing[p] || exists(p+".part") && s.active())
	}
	for end := time.Now().Add(radioIn); (arriving(path) || arriving(cached)) && time.Now().Before(end); {
		time.Sleep(20 * time.Millisecond)
	}
	if path != "" && !s.whole(path) {
		return ""
	}
	return path
}

func (s *streamer) journal() string { return filepath.Join(s.dir(), ".growing") }

// setGrowing marks path as arriving in place (or no longer) and writes .growing, the list a start
// after a crash or a pulled card deletes (dropGrowing), so a cut file is never taken for whole.
func (s *streamer) setGrowing(path string, on bool) error {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	if s.growing == nil {
		s.growing = map[string]bool{}
	}
	if on {
		s.growing[path] = true
	} else {
		delete(s.growing, path)
	}
	paths := make([]string, 0, len(s.growing))
	for p := range s.growing {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(p + "\n")
	}
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return err
	}
	return q2.WriteFile(s.journal(), []byte(b.String()))
}

// dropGrowing deletes the files .growing lists, cut by a crash or a pulled card, and the list.
// Only paths in the stream folder: the list is the app's, but it is a file on the card.
func (s *streamer) dropGrowing() {
	b, err := os.ReadFile(s.journal())
	if err != nil {
		return
	}
	for _, p := range strings.Split(string(b), "\n") {
		if p != "" && strings.HasPrefix(p, s.dir()+string(filepath.Separator)) {
			s.logf("stream: deleting %s, cut while it arrived", filepath.Base(p))
			library.Remove(s.root, p)
		}
	}
	library.Remove(s.root, s.journal())
}

// plan is the songs to have, in order: the one playing, then the songs the firmware's .ahead for
// this queue names (the songs its queue holds after the one playing, in its play order), then the
// list's songs after the one playing not yet whole, in list order: the ones its queue takes next,
// as its queue is the m3u's lines up to the first one not there yet. n (Off being 0) is how many of
// them it holds, but a song .ahead names from before the one playing that is already whole takes
// none: under Repeat All, .ahead names the next pass once the songs queued have all played, and the
// list's next must not wait behind it (the drive of 2026-11-22: a radio stopped at its fourth song).
// Without an .ahead, the list's next streamWindow.
func (r *streamRun) plan(cur int) []int {
	out := []int{cur}
	if r.s.caps.Ahead && r.s.aheadFile != "" {
		if a, ok := readAhead(r.s.aheadFile); ok && a.seq == r.playSeq {
			slots := 0 // the plan's songs after cur that take one of n's
			add := func(i int) {
				out = append(out, i)
				if i > cur || !r.s.whole(r.jobs[i].path) { // a whole song behind cur: Repeat All's next pass
					slots++
				}
			}
			for _, p := range a.paths {
				if slots >= a.n {
					break
				}
				if i := r.find(p, cur); i >= 0 && !slices.Contains(out, i) {
					add(i)
				}
			}
			// then the list's: one still to fetch, or one this run fetched that .ahead doesn't name yet
			// (its queue not yet holding it), which keeps its slot
			for i := cur + 1; i < len(r.jobs) && slots < a.n; i++ {
				if j := r.jobs[i]; j.fetch && (j.landed || !r.s.whole(j.path)) && !slices.Contains(out, i) {
					add(i)
				}
			}
			return out
		}
	}
	for i := cur + 1; i < len(r.jobs) && i <= cur+streamWindow; i++ {
		out = append(out, i)
	}
	return out
}

// find is the job playing from path: the first after cur, else the first from the list's start.
func (r *streamRun) find(path string, cur int) int {
	for i := cur + 1; i < len(r.jobs); i++ {
		if r.jobs[i].path == path {
			return i
		}
	}
	for i := r.from; i <= cur && i < len(r.jobs); i++ {
		if r.jobs[i].path == path {
			return i
		}
	}
	return -1
}

// progress is the download running now, for .stream.
type progress struct {
	job          int   // its job, -1 for none
	have, total  int64 // bytes on the card, and of the whole (0 unknown)
	movedAt      time.Time
	lastBody     string
	lastAt       time.Time
	lastArriving bool
	// For log.txt (the user's report of 2026-11-22: the progress stuck at 0.3 MB before a song started):
	began, headers, first time.Time     // the request sent, its answer's headers, the body's first byte
	stallMax              time.Duration // the longest wait for bytes
	stallSaid             bool          // the wait now running was logged (stallLog)
}

// stallLog is how long a download goes without a byte before log.txt says so: once while it waits,
// at a .stream write, and once more when bytes come again. slowStatus is a .stream write (which holds
// the lock the download's every chunk takes) long enough to say too.
const (
	stallLog   = 700 * time.Millisecond
	slowStatus = 300 * time.Millisecond
)

// status writes .stream: a line for each planned song the server sends (want the one arriving),
// at once when a line changes, and every statusEvery while one arrives. Without "ahead" nothing.
func (r *streamRun) status(plan []int, want int) {
	s := r.s
	if !s.caps.Ahead || s.statusFile == "" {
		return
	}
	var said []string // logged once the lock is let go: log.txt is on the card
	defer func() {
		for _, l := range said {
			s.logf("%s", l)
		}
	}()
	s.pmu.Lock()
	defer s.pmu.Unlock()
	held := time.Now()
	p := &r.prog
	if want >= 0 && p.job == want && !p.stallSaid && time.Since(p.movedAt) > stallLog {
		p.stallSaid = true
		said = append(said, fmt.Sprintf("stream: %q: no bytes for %d ms at %d bytes", r.songs[want].Title,
			time.Since(p.movedAt).Milliseconds(), p.have))
	}
	defer func() {
		if d := time.Since(held); d > slowStatus {
			said = append(said, fmt.Sprintf("stream: .stream written in %d ms", d.Milliseconds()))
		}
	}()
	var b strings.Builder
	fmt.Fprintf(&b, "#Q2STREAM/1\tseq=%d\n", r.playSeq)
	arriving := false
	for _, i := range plan {
		j, sg := r.jobs[i], r.songs[i]
		if !j.fetch && !j.cached {
			continue
		}
		state, have, total := "wait", int64(0), expect(sg, j.quality)
		switch {
		case i == want && p.job == i:
			state, have, arriving = "get", p.have, true
			if p.total > 0 {
				total = p.total
			}
			if time.Since(p.movedAt) >= cmpOr(s.stallAfter, stallAfter) {
				state = "stall"
			}
		case s.whole(j.path):
			state = "done"
			if st, err := os.Stat(j.path); err == nil {
				have, total = st.Size(), st.Size()
			}
		case j.away || j.failed:
			state = "fail"
		}
		fmt.Fprintf(&b, "%s\t%d\t%d\t%d\t%s\n", state, have, total, int64(sg.Duration)*1000, q2spot.Escape(j.path))
	}
	body := b.String()
	if body == p.lastBody && !(arriving && time.Since(p.lastAt) >= cmpOr(s.statusEvery, statusEvery)) {
		return
	}
	if err := q2.WriteFile(s.statusFile, []byte(body)); err != nil {
		s.logf("stream: status: %v", err)
		return
	}
	p.lastBody, p.lastAt, p.lastArriving = body, time.Now(), arriving
}

func cmpOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// counter counts the bytes a download writes into the run's progress.
type counter struct {
	r *streamRun
}

func (c counter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	now, said := time.Now(), ""
	c.r.s.pmu.Lock()
	p := &c.r.prog
	title := ""
	if p.job >= 0 && p.job < len(c.r.songs) {
		title = c.r.songs[p.job].Title
	}
	if p.first.IsZero() {
		p.first = now
		said = fmt.Sprintf("stream: %q: headers in %d ms, first byte in %d ms", title,
			p.headers.Sub(p.began).Milliseconds(), now.Sub(p.began).Milliseconds())
	} else if gap := now.Sub(p.movedAt); gap > stallLog {
		p.stallMax = max(p.stallMax, gap)
		p.stallSaid = false
		said = fmt.Sprintf("stream: %q: bytes again after %d ms at %d bytes", title, gap.Milliseconds(), p.have)
	}
	p.have += int64(len(b))
	p.movedAt = now
	c.r.s.pmu.Unlock()
	if said != "" {
		c.r.s.logf("%s", said)
	}
	return len(b), nil
}

// fetchSummary is log.txt's line at a download's end: its bytes, time, waits and rate.
func (r *streamRun) fetchSummary(title string, err error) {
	r.s.pmu.Lock()
	p := r.prog
	r.s.pmu.Unlock()
	took := time.Since(p.began)
	rate := int64(0)
	if ms := took.Milliseconds(); ms > 0 {
		rate = p.have * 1000 / ms / 1024
	}
	end := "done"
	if err != nil {
		end = "ended: " + err.Error()
	}
	r.s.logf("stream: %q: %d bytes in %d ms, %d KB/s, longest stall %d ms, %s", title, p.have,
		took.Milliseconds(), rate, p.stallMax.Milliseconds(), end)
}
