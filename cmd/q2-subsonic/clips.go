package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
	"github.com/andsuor/q2-subsonic/internal/catalogue"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Clips for Caller Ten, Q2 Muse's game (q2-coda's docs/server-library.md#clips): on firmware with
// the "clip" word the app keeps /tmp/q2-subsonic.clips, its answers, from its start to its exit,
// its header "#Q2CLIPS/1\tserver=up|down" saying whether the server answers (the Server Library's
// check: up once a ping is answered, a ping that timed out while a download got bytes leaving it
// up). The game writes /tmp/q2-subsonic.clip whole whenever what it wants changes: a header
// "#Q2CLIP/1\tseq=<n>", then a line per clip, "<slot>\t<sid>\t<from ms>\t<length ms>". For each
// line the app fetches the song as it streams one (Streaming Quality, the stream's format), from
// about the byte of <from ms> (its size times from over its length, a Range request) for <length
// ms> and clipMargin more, and writes it whole to .subsonic/clips/<sid>.<ext>. Its answer line is
// "<seq>\t<slot>\tget|done|fail\t<offset ms>\t<path>": get while it is fetched, done with the ms of
// the song the file starts at, or fail (why goes in log.txt). A server that ignores the range sends
// the song from its start: what comes before the clip's byte is read and dropped, so the file and
// its offset are the same.
//
// A line the request no longer names is stopped and its clip deleted; a new seq supersedes the
// whole request, and one with no lines deletes every clip. Only the first clipMax lines are
// fetched, the rest answered fail, so the folder holds those and the files being written.
//
// A clip is never part of the stream run: no .play, .stream, .queue, Stream Cache or play report.
// It is fetched through the client's download connections, so the pages' calls never wait behind
// it, and takes no network slot, so a song of the stream never waits for it; while the song the
// stream fetches arrives slower than it plays (rateNote), a clip's reads wait.

const (
	clipMax    = 2               // clips fetched for a request at most
	clipMargin = 3 * time.Second // audio fetched past a clip's length
	clipIn     = 60 * time.Second
	clipPoll   = 100 * time.Millisecond // a clip waiting on a slow stream looks again
)

var clipHead = regexp.MustCompile(`^#Q2CLIP/1\tseq=(\d+)$`)

// clipJob is one line of the request and how far its clip has come.
type clipJob struct {
	seq, slot, sid string
	from, length   int64 // ms
	state          string
	off            int64  // ms of the song the file starts at (done)
	path, part     string // the clip's file and the file it is written to, once its format is known
	gone           bool   // no longer asked for: its fetch ends and keeps nothing
	cancel         context.CancelFunc
}

func (j *clipJob) key() string {
	return fmt.Sprintf("%s\t%s\t%s\t%d\t%d", j.seq, j.slot, j.sid, j.from, j.length)
}

type clipState struct {
	mu      sync.Mutex
	raw     string // the request as last read
	read    bool
	seq     string
	jobs    []*clipJob // the request's lines, in its order
	running int        // fetches not yet ended, gone ones too
	written string     // the answer file as last written
}

func (d *daemon) clipReqPath() string { return filepath.Join(d.tmp, "q2-"+appID+".clip") }
func (d *daemon) clipAnsPath() string { return filepath.Join(d.tmp, "q2-"+appID+".clips") }
func (d *daemon) clipsDir() string    { return filepath.Join(d.dir, "clips") }

// clipServer is the header's server=: up while the Server Library runs and its check was answered.
func (d *daemon) clipServer() string {
	if !d.slActive() {
		return "down"
	}
	d.sl.mu.Lock()
	defer d.sl.mu.Unlock()
	if d.sl.up == 1 {
		return "up"
	}
	return "down"
}

// clipInit writes the answer file's header and deletes the clips a run before left.
func (d *daemon) clipInit() {
	if !d.caps.Clip {
		return
	}
	d.clipSweep(nil)
	d.clip.mu.Lock()
	d.clipFlush()
	d.clip.mu.Unlock()
}

// clipClose stops the fetches and deletes the answer file: the game streams no clip without it.
func (d *daemon) clipClose() {
	if !d.caps.Clip {
		return
	}
	d.clip.mu.Lock()
	defer d.clip.mu.Unlock()
	for _, j := range d.clip.jobs {
		j.gone = true
		if j.cancel != nil {
			j.cancel()
		}
	}
	d.clip.jobs = nil
	os.Remove(d.clipAnsPath())
	d.clip.written = ""
}

// clipBusy is whether a clip is being fetched.
func (d *daemon) clipBusy() bool {
	d.clip.mu.Lock()
	defer d.clip.mu.Unlock()
	return d.clip.running > 0
}

// clipTick keeps the header's server= current and takes a new request.
func (d *daemon) clipTick() {
	if !d.caps.Clip {
		return
	}
	b, err := os.ReadFile(d.clipReqPath())
	if err != nil {
		b = nil // no request: no clip wanted
	}
	st := &d.clip
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.read || string(b) != st.raw {
		st.read, st.raw = true, string(b)
		d.clipTake(string(b))
		d.mu.Lock()
		d.lastReq = time.Now() // the game is using the app
		d.mu.Unlock()
	}
	d.clipFlush()
}

// clipTake replaces the jobs with req's lines; st.mu is held.
func (d *daemon) clipTake(req string) {
	st := &d.clip
	lines := strings.Split(strings.TrimSuffix(req, "\n"), "\n")
	seq := ""
	if req != "" {
		m := clipHead.FindStringSubmatch(strings.TrimSuffix(lines[0], "\r"))
		if m == nil {
			d.log.Printf("clips: a request with no header ignored")
			return
		}
		seq = m[1]
	}
	old := map[string]*clipJob{}
	for _, j := range st.jobs {
		old[j.key()] = j
	}
	var jobs []*clipJob
	for _, l := range lines[1:] {
		j, ok := parseClip(seq, strings.TrimSuffix(l, "\r"))
		if !ok {
			d.log.Printf("clips: a bad line ignored: %q", l)
			continue
		}
		if o := old[j.key()]; o != nil {
			delete(old, j.key())
			j = o
		} else if len(jobs) >= clipMax {
			j.state = "fail"
			d.log.Printf("clips: %s failed: more than %d clips asked at once", j.sid, clipMax)
		} else {
			d.clipStart(j)
		}
		jobs = append(jobs, j)
	}
	for _, j := range old {
		j.gone = true
		if j.cancel != nil {
			j.cancel()
		}
		if j.state == "get" {
			d.log.Printf("clips: %s no longer asked for: its fetch stopped", j.sid)
		}
	}
	st.seq, st.jobs = seq, jobs
	d.clipSweep(jobs)
}

// parseClip is one line of the request: "<slot>\t<sid>\t<from ms>\t<length ms>".
func parseClip(seq, l string) (*clipJob, bool) {
	f := strings.Split(l, "\t")
	if len(f) != 4 || !catalogue.CoverID(f[1]) {
		return nil, false
	}
	slot, err1 := strconv.Atoi(f[0])
	from, err2 := strconv.ParseInt(f[2], 10, 64)
	length, err3 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || slot < 0 || from < 0 || length <= 0 {
		return nil, false
	}
	return &clipJob{seq: seq, slot: f[0], sid: f[1], from: from, length: length}, true
}

// clipStart fetches j in the background; st.mu is held.
func (d *daemon) clipStart(j *clipJob) {
	ctx, cancel := context.WithTimeout(context.Background(), clipIn)
	j.state, j.cancel = "get", cancel
	d.clip.running++
	d.spawn(func() {
		defer cancel()
		d.clipFetch(ctx, j)
		d.clip.mu.Lock()
		d.clip.running--
		d.clip.mu.Unlock()
	})
}

// clipSweep deletes every file in the clips folder but jobs' clips and the files being written.
func (d *daemon) clipSweep(jobs []*clipJob) {
	keep := map[string]bool{}
	for _, j := range jobs {
		if j.path != "" {
			keep[filepath.Base(j.path)] = true
			keep[filepath.Base(j.part)] = true
		}
	}
	ents, _ := os.ReadDir(d.clipsDir())
	for _, e := range ents {
		if !keep[e.Name()] {
			if err := library.Remove(d.card, filepath.Join(d.clipsDir(), e.Name())); err != nil {
				d.log.Printf("clips: %v", err)
			}
		}
	}
}

// clipFlush writes the answer file when it changed; st.mu is held.
func (d *daemon) clipFlush() {
	var b strings.Builder
	b.WriteString("#Q2CLIPS/1\tserver=" + d.clipServer() + "\n")
	for _, j := range d.clip.jobs {
		path := j.path
		if j.state == "fail" {
			path = ""
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\n", j.seq, j.slot, j.state, j.off, path)
	}
	if b.String() == d.clip.written {
		return
	}
	if err := q2.WriteFile(d.clipAnsPath(), []byte(b.String())); err != nil {
		d.log.Printf("clips: %v", err)
		return
	}
	d.clip.written = b.String()
}

// clipEnd records how j's fetch ended, unless it is no longer asked for.
func (d *daemon) clipEnd(j *clipJob, state string, off int64) {
	d.clip.mu.Lock()
	defer d.clip.mu.Unlock()
	if j.gone {
		return
	}
	j.state, j.off = state, off
	d.clipFlush()
}

// clipTotal is the bytes of sg streamed at quality q: a transcode's its length at the bit rate.
func clipTotal(sg subsonic.Song, q string) int64 {
	switch q {
	case "mp3-320":
		return int64(sg.Duration) * 320000 / 8
	case "mp3-128":
		return int64(sg.Duration) * 128000 / 8
	}
	return sg.Size
}

// slowReader waits before each Read while the stream's song arrives slower than it plays.
type slowReader struct {
	r    io.Reader
	ctx  context.Context
	slow func() bool
	said func()
}

func (s slowReader) Read(p []byte) (int, error) {
	if err := s.wait(); err != nil {
		return 0, err
	}
	return s.r.Read(p)
}

// wait returns once the stream's song no longer arrives slower than it plays, or ctx ends.
func (s slowReader) wait() error {
	for s.slow() {
		s.said()
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(clipPoll):
		}
	}
	return nil
}

// clipFetch fetches j's clip and answers it.
func (d *daemon) clipFetch(ctx context.Context, j *clipJob) {
	begun := time.Now()
	d.log.Printf("clips: %s asked, from %d ms for %d ms (seq %s, slot %s)", j.sid, j.from, j.length, j.seq, j.slot)
	fail := func(why string) {
		d.clip.mu.Lock()
		gone := j.gone
		d.clip.mu.Unlock()
		if gone {
			return // its line went: said when it went
		}
		d.log.Printf("clips: %s failed: %s", j.sid, why)
		d.clipEnd(j, "fail", 0)
	}
	if d.clipServer() != "up" {
		fail("the server is down")
		return
	}
	cl := d.client()
	if cl == nil {
		fail("signed out")
		return
	}
	lost := func(err error) {
		if subsonic.ConnectionLost(err) {
			d.slServer(err)
			d.clip.mu.Lock()
			d.clipFlush()
			d.clip.mu.Unlock()
		}
	}
	sg, err := cl.Song(ctx, j.sid)
	if err != nil {
		lost(err)
		fail(applog.Redact(err.Error()))
		return
	}
	ext, q := d.stream.format(sg, d.settings().stream())
	total, lenMs := clipTotal(sg, q), int64(sg.Duration)*1000
	if total <= 0 || lenMs <= 0 {
		fail("its size or length unknown")
		return
	}
	at := min(total*j.from/lenMs, total-1)
	need := total * (j.length + clipMargin.Milliseconds()) / lenMs
	path := filepath.Join(d.clipsDir(), j.sid+"."+ext)
	d.clip.mu.Lock()
	if j.gone {
		d.clip.mu.Unlock()
		return
	}
	j.path, j.part = path, fmt.Sprintf("%s.%s-%s.part", path, j.seq, j.slot) // its own, should a line gone have its sid
	d.clipFlush()
	d.clip.mu.Unlock()
	if !library.Owned(d.card, j.part, nil) {
		fail("refusing a path outside the app's folder")
		return
	}
	said := false
	slow := slowReader{ctx: ctx, slow: d.stream.arrivingSlow, said: func() {
		if !said {
			said = true
			d.log.Printf("clips: %s waits: the song streaming arrives slower than it plays", j.sid)
		}
	}}
	if err := slow.wait(); err != nil { // before the request too
		fail(err.Error())
		return
	}
	resp, err := cl.Get(ctx, cl.PlayURL(j.sid, q), at)
	if err != nil {
		lost(err)
		fail(applog.Redact(err.Error()))
		return
	}
	defer resp.Body.Close()
	slow.r = resp.Body
	ranged, start := resp.StatusCode == 206, at
	if ranged {
		if b, n, ok := contentRange(resp.Header.Get("Content-Range")); ok {
			start = b
			if n > 0 {
				total = n
			}
		}
	} else {
		if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
		start = min(total*j.from/lenMs, total-1)
		if _, err := io.CopyN(io.Discard, slow, start); err != nil {
			lost(err)
			fail("reading to its start: " + applog.Redact(err.Error()))
			return
		}
	}
	if err := os.MkdirAll(d.clipsDir(), 0o755); err != nil {
		fail(err.Error())
		return
	}
	f, err := os.Create(j.part)
	if err != nil {
		fail(err.Error())
		return
	}
	n, err := io.CopyN(f, slow, need)
	if err == io.EOF && n > 0 {
		err = nil // the song ended first
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	d.clip.mu.Lock()
	if err == nil && !j.gone {
		err = os.Rename(j.part, j.path)
	}
	if err != nil || j.gone {
		library.Remove(d.card, j.part)
	}
	gone := j.gone
	d.clip.mu.Unlock()
	if gone {
		return
	}
	if err != nil {
		lost(err)
		fail(applog.Redact(err.Error()))
		return
	}
	how := "the range honoured"
	if !ranged {
		how = fmt.Sprintf("the range ignored, %d bytes read to its start", start)
	}
	off := start * lenMs / total
	d.log.Printf("clips: %s fetched, %d bytes in %d ms, %s; the file starts at %d ms", j.sid, n,
		time.Since(begun).Milliseconds(), how, off)
	d.clipEnd(j, "done", off)
}

// contentRange is a 206's "bytes <first>-<last>/<size>": first, and size (0 when "*").
func contentRange(v string) (first, size int64, ok bool) {
	v, ok = strings.CutPrefix(v, "bytes ")
	r, s, ok2 := strings.Cut(v, "/")
	a, _, ok3 := strings.Cut(r, "-")
	if !ok || !ok2 || !ok3 {
		return 0, 0, false
	}
	first, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	size, _ = strconv.ParseInt(s, 10, 64)
	return first, size, true
}
