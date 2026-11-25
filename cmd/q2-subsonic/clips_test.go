package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/fakesonic"
)

// newClipRig is a daemon on firmware with the clip word, signed in to a server of four 200 s songs
// of 6,000,000 bytes (30,000 bytes a second), the server checked and up.
func newClipRig(t *testing.T, caps string) *catRig {
	t.Helper()
	songs := fakesonic.Library(4)
	for i := range songs {
		songs[i].Size, songs[i].Duration = 6_000_000, 200
	}
	r := newCatRig(t, caps, songs)
	r.clipInit()
	r.check(t)
	r.clipTick()
	t.Cleanup(r.clipClose)
	return r
}

// ask writes the firmware's request whole, as its clip thread does, and runs a tick of it.
func (r *catRig) ask(t *testing.T, req string) {
	t.Helper()
	p := filepath.Join(r.tmp, "q2-subsonic.clip")
	if err := os.WriteFile(p+".tmp", []byte(req), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		t.Fatal(err)
	}
	r.clipTick()
}

// answers is the app's answer file, its lines.
func (r *catRig) answers(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.tmp, "q2-subsonic.clips"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func (r *catRig) clipFiles() []string {
	ents, _ := os.ReadDir(filepath.Join(r.dir, "clips"))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// checkClip says whether the clip at path holds the stream's bytes from at, as many as 28 s take
// at bps bytes a second: 30,000 for the rig's originals, 24,000 for a 192 kbps transcode.
func checkClip(t *testing.T, path string, at, bps int64) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := bps * (25_000 + clipMargin.Milliseconds()) / 1000; int64(len(b)) != want {
		t.Fatalf("%s: %d bytes, want %d", path, len(b), want)
	}
	for i := range int64(len(b)) {
		if b[i] != fakesonic.Media(at+i) {
			t.Fatalf("%s: byte %d is the song's %d, not %d", path, i, b[i], at+i)
		}
	}
}

// asked is what the server was asked for its songs, sorted.
func (r *catRig) asked() []string {
	r.f.Mu.Lock()
	defer r.f.Mu.Unlock()
	a := slices.Clone(r.f.Asked)
	slices.Sort(a)
	return a
}

func (r *catRig) logHas(t *testing.T, s ...string) {
	t.Helper()
	log, _ := os.ReadFile(r.file("log.txt"))
	for _, x := range s {
		if !strings.Contains(string(log), x) {
			t.Fatalf("no %q in:\n%s", x, log)
		}
	}
}

// suffix makes the rig's song id a file of that type on the server.
func (r *catRig) suffix(id, sfx string) {
	r.f.Mu.Lock()
	defer r.f.Mu.Unlock()
	for i := range r.f.Songs {
		if r.f.Songs[i].ID == id {
			r.f.Songs[i].Suffix = sfx
		}
	}
}

// A request for two clips of FLAC originals: each a 192 kbps MP3 transcode from the second of its
// start (timeOffset), with a header of its own, written whole to the clips folder, and answered
// done with its start in the song.
func TestClipsTwoInPlace(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up"}) {
		t.Fatal("at start:", a)
	}
	r.ask(t, "#Q2CLIP/1\tseq=7\n0\ts1\t60000\t25000\n1\ts2\t90000\t25000\n")
	r.bg.Wait()
	dir := filepath.Join(r.dir, "clips")
	want := []string{"#Q2CLIPS/1\tserver=up",
		"7\t0\tdone\t60000\t" + filepath.Join(dir, "s1.mp3"),
		"7\t1\tdone\t90000\t" + filepath.Join(dir, "s2.mp3")}
	if a := r.answers(t); !slices.Equal(a, want) {
		t.Fatalf("answers %q, want %q", a, want)
	}
	checkClip(t, filepath.Join(dir, "s1.mp3"), 60*24_000, 24_000)
	checkClip(t, filepath.Join(dir, "s2.mp3"), 90*24_000, 24_000)
	if a := r.asked(); !slices.Equal(a, []string{"s1 format=mp3 timeOffset=60 from=0", "s2 format=mp3 timeOffset=90 from=0"}) {
		t.Fatal(a)
	}
	if !slices.Equal(r.clipFiles(), []string{"s1.mp3", "s2.mp3"}) {
		t.Fatal(r.clipFiles())
	}
	r.logHas(t, "clips: s1 fetched, 672000 bytes", "a 192 kbps MP3 from 60 s")
}

// An M4A original is transcoded the same way: never a cut of the file, which has no header there.
func TestClipsM4ATranscoded(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.suffix("s2", "m4a")
	r.ask(t, "#Q2CLIP/1\tseq=1\n0\ts2\t30000\t25000\n")
	r.bg.Wait()
	path := filepath.Join(r.dir, "clips", "s2.mp3")
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up", "1\t0\tdone\t30000\t" + path}) {
		t.Fatal(a)
	}
	checkClip(t, path, 30*24_000, 24_000)
}

// An MP3 original is cut from the file itself, which decodes from any frame: a Range request from
// the byte of its start.
func TestClipsMP3OriginalRanged(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.suffix("s3", "mp3")
	r.ask(t, "#Q2CLIP/1\tseq=3\n0\ts3\t60000\t25000\n")
	r.bg.Wait()
	path := filepath.Join(r.dir, "clips", "s3.mp3")
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up", "3\t0\tdone\t60000\t" + path}) {
		t.Fatal(a)
	}
	checkClip(t, path, 1_800_000, 30_000)
	if a := r.asked(); !slices.Equal(a, []string{"s3 format=raw timeOffset= from=1800000"}) {
		t.Fatal(a)
	}
	r.logHas(t, "the range honoured")
}

// A server that ignores Range sends the MP3 from its start: the app reads up to the clip's start,
// keeps what follows, and answers with where its file starts all the same.
func TestClipsServerIgnoresRange(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.suffix("s3", "mp3")
	r.f.Mu.Lock()
	r.f.NoRange = true
	r.f.Mu.Unlock()
	r.ask(t, "#Q2CLIP/1\tseq=3\n0\ts3\t60000\t25000\n")
	r.bg.Wait()
	path := filepath.Join(r.dir, "clips", "s3.mp3")
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up", "3\t0\tdone\t60000\t" + path}) {
		t.Fatal(a)
	}
	checkClip(t, path, 1_800_000, 30_000)
	r.logHas(t, "the range ignored")
}

// A server that transcodes but ignores timeOffset, or refuses it, sends the MP3 from the song's
// start: the app drops what comes before the clip's byte (an MP3 decodes from any frame).
func TestClipsTimeOffsetIgnoredOrRefused(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		r := newClipRig(t, "playlist catalogue clip")
		r.f.Mu.Lock()
		r.f.NoTimeOffset, r.f.RejectTimeOffset = !refuse, refuse
		r.f.Mu.Unlock()
		r.ask(t, "#Q2CLIP/1\tseq=4\n0\ts1\t60000\t25000\n")
		r.bg.Wait()
		path := filepath.Join(r.dir, "clips", "s1.mp3")
		if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up", "4\t0\tdone\t60000\t" + path}) {
			t.Fatal(refuse, a)
		}
		checkClip(t, path, 60*24_000, 24_000)
		if refuse {
			if a := r.asked(); !slices.Equal(a, []string{"s1 format=mp3 timeOffset= from=0", "s1 format=mp3 timeOffset=60 from=0"}) {
				t.Fatal(a)
			}
			r.logHas(t, "timeOffset refused")
		} else {
			r.logHas(t, "timeOffset ignored")
		}
	}
}

// A server that doesn't transcode sends the original: a FLAC or M4A cut mid-way has no header the
// player could start from, so the clip fails and nothing is kept.
func TestClipsNoTranscodeFails(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.suffix("s2", "m4a")
	r.f.Mu.Lock()
	r.f.NoTranscode = true
	r.f.Mu.Unlock()
	r.ask(t, "#Q2CLIP/1\tseq=6\n0\ts1\t60000\t25000\n1\ts2\t60000\t25000\n")
	r.bg.Wait()
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up", "6\t0\tfail\t0\t", "6\t1\tfail\t0\t"}) {
		t.Fatal(a)
	}
	if len(r.clipFiles()) != 0 {
		t.Fatal(r.clipFiles())
	}
	r.logHas(t, "clips: s1 failed: the server sent audio/flac, not an MP3", "clips: s2 failed: the server sent audio/m4a, not an MP3")
}

// A new seq supersedes the request before it: its fetch still running is cancelled, and its clip
// neither answered nor kept.
func TestClipsNewSeqCancels(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.f.Mu.Lock()
	r.f.Hold, r.f.HoldBytes = "s1", 64<<10
	r.f.Mu.Unlock()
	r.ask(t, "#Q2CLIP/1\tseq=1\n0\ts1\t60000\t25000\n")
	for i := 0; ; i++ {
		r.f.Mu.Lock()
		n := len(r.f.Asked)
		r.f.Mu.Unlock()
		if n == 1 {
			break
		}
		if i > 500 {
			t.Fatal("the first clip never asked of the server")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a := r.answers(t); len(a) != 2 || !strings.HasPrefix(a[1], "1\t0\tget\t") {
		t.Fatal("while it is fetched:", a)
	}
	r.ask(t, "#Q2CLIP/1\tseq=2\n0\ts2\t30000\t25000\n")
	r.bg.Wait()
	r.f.Mu.Lock()
	cut := r.f.Cut
	r.f.Mu.Unlock()
	path := filepath.Join(r.dir, "clips", "s2.mp3")
	if a := r.answers(t); cut != 1 || !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up", "2\t0\tdone\t30000\t" + path}) {
		t.Fatal(cut, a)
	}
	if !slices.Equal(r.clipFiles(), []string{"s2.mp3"}) {
		t.Fatal(r.clipFiles())
	}
}

// A request with no lines (the contest over) deletes every clip.
func TestClipsEmptyRequestDeletes(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.ask(t, "#Q2CLIP/1\tseq=7\n0\ts1\t60000\t25000\n1\ts2\t90000\t25000\n")
	r.bg.Wait()
	if len(r.clipFiles()) != 2 {
		t.Fatal(r.clipFiles())
	}
	r.ask(t, "#Q2CLIP/1\tseq=7\n")
	r.bg.Wait()
	if a := r.answers(t); len(r.clipFiles()) != 0 || !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=up"}) {
		t.Fatal(r.clipFiles(), a)
	}
}

// The folder holds the clips the request names, two at most, and what is being written: a third
// line is answered fail, a file the request doesn't name goes, and so does a clip it stops naming.
func TestClipsFolderCapped(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	dir := filepath.Join(r.dir, "clips")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "old.mp3"), []byte("left by a run before"), 0o644)
	r.ask(t, "#Q2CLIP/1\tseq=5\n0\ts1\t60000\t25000\n1\ts2\t60000\t25000\n2\ts3\t60000\t25000\n")
	r.bg.Wait()
	a := r.answers(t)
	if len(a) != 4 || !strings.HasPrefix(a[1], "5\t0\tdone\t") || !strings.HasPrefix(a[2], "5\t1\tdone\t") || a[3] != "5\t2\tfail\t0\t" {
		t.Fatal(a)
	}
	if !slices.Equal(r.clipFiles(), []string{"s1.mp3", "s2.mp3"}) {
		t.Fatal(r.clipFiles())
	}
	r.ask(t, "#Q2CLIP/1\tseq=5\n1\ts2\t60000\t25000\n3\ts4\t60000\t25000\n")
	r.bg.Wait()
	if !slices.Equal(r.clipFiles(), []string{"s2.mp3", "s4.mp3"}) {
		t.Fatal(r.clipFiles())
	}
	if a := r.answers(t); len(a) != 3 || !strings.HasPrefix(a[1], "5\t1\tdone\t") || !strings.HasPrefix(a[2], "5\t3\tdone\t") {
		t.Fatal(a)
	}
}

// A clip fetch leaves the user's stream alone: its run goes on, nothing is reported as played, and
// none of the stream's files is written. While the user's song arrives slower than it plays, the
// clip waits, and goes on once it no longer does.
func TestClipsLeaveTheStreamAlone(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	done := make(chan struct{})
	r.stream.mu.Lock()
	r.stream.done, r.stream.cancel = done, func() { close(done) }
	r.stream.mu.Unlock()
	r.stream.xfer.Store(true)
	r.stream.slow.Store(true)
	r.ask(t, "#Q2CLIP/1\tseq=9\n0\ts1\t60000\t25000\n")
	time.Sleep(300 * time.Millisecond)
	if n := r.calls("stream"); n != 0 {
		t.Fatal("a clip fetched while the user's song arrives slowly:", n)
	}
	r.stream.slow.Store(false)
	r.bg.Wait()
	if a := r.answers(t); len(a) != 2 || !strings.HasPrefix(a[1], "9\t0\tdone\t") {
		t.Fatal(a)
	}
	if !r.stream.active() || r.calls("scrobble") != 0 {
		t.Fatal("the stream stopped, or a play reported:", r.stream.active(), r.calls("scrobble"))
	}
	for _, f := range []string{"q2-subsonic.play", "q2-subsonic.stream", "q2-subsonic.queue"} {
		if exists(filepath.Join(r.tmp, f)) {
			t.Fatal(f, "written")
		}
	}
	if exists(filepath.Join(r.card, ".subsonic", "stream")) {
		t.Fatal("the stream's folder written")
	}
}

// With the server down the header says so, and a clip asked for all the same is answered fail
// without a request for it.
func TestClipsServerDown(t *testing.T) {
	r := newClipRig(t, "playlist catalogue clip")
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.check(t)
	r.clipTick()
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=down"}) {
		t.Fatal(a)
	}
	r.ask(t, "#Q2CLIP/1\tseq=4\n0\ts1\t60000\t25000\n")
	r.bg.Wait()
	if a := r.answers(t); !slices.Equal(a, []string{"#Q2CLIPS/1\tserver=down", "4\t0\tfail\t0\t"}) {
		t.Fatal(a)
	}
	if n := r.calls("stream"); n != 0 {
		t.Fatal(n)
	}
	log, _ := os.ReadFile(r.file("log.txt"))
	if !strings.Contains(string(log), "clips: s1 failed: the server is down") {
		t.Fatal(string(log))
	}
}

// Without the firmware's clip word the app writes no answer file and fetches nothing; with it, the
// file goes at the app's exit.
func TestClipsNeedTheWord(t *testing.T) {
	r := newClipRig(t, "playlist catalogue")
	r.ask(t, "#Q2CLIP/1\tseq=7\n0\ts1\t60000\t25000\n")
	r.bg.Wait()
	if exists(filepath.Join(r.tmp, "q2-subsonic.clips")) || r.calls("stream") != 0 {
		t.Fatal("clips without the word")
	}
	w := newClipRig(t, "playlist catalogue clip")
	w.clipClose()
	if exists(filepath.Join(w.tmp, "q2-subsonic.clips")) {
		t.Fatal("the answer file kept at exit")
	}
}

// Each kind of clip decodes from its first byte: FLAC and M4A originals transcoded to MP3 from
// their second (ffmpeg standing in for the server's transcoder), an MP3 original cut mid-file with
// Range. ffmpeg's decoders are the ones Q2 Muse's player is built from (q2-coda's tools/player.py).
// Skipped without ffmpeg.
func TestClipsDecode(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	src := t.TempDir()
	files := map[string][]byte{}
	for id, args := range map[string][]string{"s1": {"-c:a", "flac", "-f", "flac"}, "s2": {"-c:a", "aac", "-b:a", "256k", "-f", "ipod"},
		"s3": {"-c:a", "libmp3lame", "-b:a", "320k", "-f", "mp3"}} {
		p := filepath.Join(src, id)
		cmd := exec.Command(ffmpeg, append([]string{"-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=60",
			"-ac", "2", "-ar", "44100"}, append(args, p)...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(id, err, string(out))
		}
		files[id], _ = os.ReadFile(p)
	}
	r := newClipRig(t, "playlist catalogue clip")
	r.f.Mu.Lock()
	for i, sfx := range []string{"flac", "m4a", "mp3"} {
		s := &r.f.Songs[i]
		s.Suffix, s.Duration, s.Size = sfx, 60, int64(len(files[s.ID]))
	}
	r.f.Files = files
	r.f.Transcoder = func(id string, kbps, at int) []byte {
		out, err := exec.Command(ffmpeg, "-v", "error", "-ss", strconv.Itoa(at), "-i", filepath.Join(src, id),
			"-c:a", "libmp3lame", "-b:a", strconv.Itoa(kbps)+"k", "-f", "mp3", "-").Output()
		if err != nil {
			t.Error(err)
		}
		return out
	}
	r.f.Mu.Unlock()
	for seq, id := range []string{"s1", "s2", "s3"} {
		r.ask(t, fmt.Sprintf("#Q2CLIP/1\tseq=%d\n0\t%s\t20000\t25000\n", seq+1, id))
		r.bg.Wait()
		path := filepath.Join(r.dir, "clips", id+".mp3")
		if a := r.answers(t); len(a) != 2 || !strings.HasPrefix(a[1], fmt.Sprintf("%d\t0\tdone\t", seq+1)) {
			t.Fatal(id, a)
		}
		// FFmpeg's integer MP3 decoder, the player's (tools/player.py); a cut original's first frame
		// may point back into a bit reservoir the cut left out, which costs that frame alone
		out, err := exec.Command(ffmpeg, "-v", "error", "-c:a", "mp3", "-i", path, "-f", "null", "-").CombinedOutput()
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if err != nil || len(out) > 0 && (id != "s3" || len(lines) > 1 || !strings.Contains(lines[0], "backstep")) {
			t.Fatalf("%s: %v %s", id, err, out)
		}
		probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,duration", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(id, err)
		}
		f := strings.Split(strings.TrimSpace(string(probe)), ",")
		sec, _ := strconv.ParseFloat(f[len(f)-1], 64)
		if f[0] != "mp3" || sec < 25 || sec > 30 {
			t.Fatalf("%s: %q", id, probe)
		}
		t.Logf("%s: %s", id, strings.TrimSpace(string(probe)))
	}
}
