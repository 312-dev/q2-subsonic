package pace

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		text string
		want State
	}{
		{"paced=1 rate=204800 freq=2412\n", State{Paced: true, Rate: 204800}},
		{"paced=0 rate=0 freq=5180\n", State{}},
		{"paced=1 rate=0 freq=2412\n", State{}},            // no rate: nothing to hold to
		{"paced=1\n", State{}},                             // cut short
		{"paced=1 rate=999999999999 freq=2412\n", State{}}, // past maxRate: not q2guard's
		{"", State{}},
	} {
		if got := Parse([]byte(c.text)); got != c.want {
			t.Errorf("%q: %+v, want %+v", c.text, got, c.want)
		}
	}
}

// clock is a gate's time: each Sleep moves it on and is kept.
type clock struct {
	now   time.Time
	slept time.Duration
}

func (c *clock) gate(path string) *Gate {
	g := New(path)
	g.Every = 0
	g.Now = func() time.Time { return c.now }
	g.Sleep = func(ctx context.Context, d time.Duration) error {
		c.now = c.now.Add(d)
		c.slept += d
		return ctx.Err()
	}
	return g
}

func state(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// One MB through a paced reader at 200 KB/s takes about five seconds of its clock, its first second's worth not
// held; unpaced, none.
func TestAReaderIsHeldToTheRateWhilePaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q2-bt-coex")
	state(t, path, "paced=1 rate=204800 freq=2412\n")
	c := &clock{now: time.Unix(1795464061, 0)}
	body := bytes.Repeat([]byte{7}, 1<<20)
	n, err := io.Copy(io.Discard, c.gate(path).Reader(context.Background(), bytes.NewReader(body), nil))
	if err != nil || n != 1<<20 {
		t.Fatal(n, err)
	}
	if c.slept < 4*time.Second || c.slept > 5200*time.Millisecond {
		t.Fatal("held", c.slept)
	}
	state(t, path, "paced=0 rate=0 freq=5180\n")
	c.slept = 0
	io.Copy(io.Discard, c.gate(path).Reader(context.Background(), bytes.NewReader(body), nil))
	if c.slept != 0 {
		t.Fatal("held unpaced", c.slept)
	}
}

// free says when a read may go unheld (the song playing, behind where it plays): none of those reads is held, and
// pacing starts afresh when it says otherwise.
func TestFreeReadsAreNotHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q2-bt-coex")
	state(t, path, "paced=1 rate=102400 freq=2412\n")
	c := &clock{now: time.Unix(1795464061, 0)}
	free := true
	r := c.gate(path).Reader(context.Background(), bytes.NewReader(make([]byte, 1<<20)), func() bool { return free })
	if _, err := io.CopyN(io.Discard, r, 512<<10); err != nil || c.slept != 0 {
		t.Fatal(err, c.slept)
	}
	free = false
	io.Copy(io.Discard, r)
	if c.slept < 4*time.Second || c.slept > 5200*time.Millisecond { // the second half at 100 KB/s
		t.Fatal("held", c.slept)
	}
}

// No file (firmware without it, or q2guard not running) is no pacing; a change of the file reaches a running reader
// at its next read once Every has passed, and Changed hears of it once.
func TestTheFileIsReadAgainEverySecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q2-bt-coex")
	c := &clock{now: time.Unix(1795464061, 0)}
	g := c.gate(path)
	g.Every = time.Second
	var heard []State
	g.Changed = func(s State) { heard = append(heard, s) }
	if g.Paced() {
		t.Fatal("paced with no file")
	}
	state(t, path, "paced=1 rate=204800 freq=2412\n")
	if g.Paced() {
		t.Fatal("read again before Every")
	}
	c.now = c.now.Add(time.Second)
	if !g.Paced() || g.State().Rate != 204800 {
		t.Fatal("not read again after Every")
	}
	if len(heard) != 1 || !heard[0].Paced {
		t.Fatal(heard)
	}
	var none *Gate
	if none.Paced() || none.Reader(context.Background(), bytes.NewReader([]byte("x")), nil) == nil {
		t.Fatal("a nil gate paces")
	}
}

// A cancel ends a held read's wait: the reader returns the context's error.
func TestACancelEndsTheWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q2-bt-coex")
	state(t, path, "paced=1 rate=1024 freq=2412\n")
	g := New(path)
	ctx, cancel := context.WithCancel(context.Background())
	r := g.Reader(ctx, bytes.NewReader(make([]byte, 64<<10)), nil)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := io.Copy(io.Discard, r)
	if err != context.Canceled || time.Since(start) > 2*time.Second {
		t.Fatal(err, time.Since(start))
	}
}
