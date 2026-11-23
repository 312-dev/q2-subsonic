package syncer

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/plan"
)

// The manifest's writes in a sync onto a card that holds many synced songs already: each save
// rewrites the whole manifest (and fsyncs it on the Q2's card). 200 downloads at 5 s each (the
// clock moved 5 s per download) onto a card with 5,000 songs recorded.
func TestASyncsManifestWritesStaySmall(t *testing.T) {
	_, r, root := setup(t)
	m := &library.Manifest{Songs: map[string]*library.Entry{}}
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("old%04d", i)
		m.Songs[id] = &library.Entry{ID: id, Path: filepath.Join(root, "Music", "Artist", "Album", id+" - A song title.flac"),
			Quality: "original", Size: 30000000, Items: []string{"album:al" + id}, Meta: "m", Album: "al" + id}
	}
	if err := m.Save(root, filepath.Join(root, ".subsonic", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	var saves int
	var bytes int64
	r.onOp = func(op, _ string) {
		switch op {
		case "download":
			mu.Lock()
			now = now.Add(5 * time.Second)
			mu.Unlock()
		case "save":
			saves++
			if st, err := os.Stat(filepath.Join(root, ".subsonic", "manifest.json")); err == nil {
				bytes += st.Size()
			}
		}
	}
	var ws []plan.Want
	for i := 0; i < 200; i++ {
		ws = append(ws, want(root, fmt.Sprint("s", i), "mp3-320"))
	}
	r.Start(plan.Plan{Add: ws})
	if s := wait(t, r); s.State != "done" || s.Done != 200 {
		t.Fatalf("%+v", s)
	}
	t.Logf("%d saves, about %.1f MB written to the manifest", saves, float64(bytes)/1e6)
	if mm := manifest(t, root); len(mm.Songs) != 5200 {
		t.Fatalf("%d songs recorded", len(mm.Songs))
	}
	if saves > 6 {
		t.Errorf("%d manifest saves (about %.1f MB) for 200 downloads", saves, float64(bytes)/1e6)
	}
}

// A cover goes to the card as it is read, a chunk at a time, never whole in RAM; past the limit
// nothing is written.
func TestACoverIsWrittenAsItComes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cover.jpg")
	src := &chunks{left: 3 << 20}
	if err := writeLimited(path, src, 16<<20); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() != 3<<20 {
		t.Fatalf("%v %v", st, err)
	}
	if src.biggest > 64<<10 {
		t.Errorf("read %d bytes at once", src.biggest)
	}
	if err := writeLimited(filepath.Join(dir, "big.jpg"), &chunks{left: 2 << 20}, 1<<20); err == nil {
		t.Fatal("past the limit written")
	}
	if _, err := os.Stat(filepath.Join(dir, "big.jpg")); !os.IsNotExist(err) {
		t.Fatal("a cover past the limit left on the card")
	}
}

type chunks struct{ left, biggest int }

func (c *chunks) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.left)
	c.biggest = max(c.biggest, n)
	c.left -= n
	return n, nil
}
