package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/fw"
	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/plan"
	"github.com/andsuor/q2-subsonic/internal/selection"
)

// Without the firmware's "storage" word the app writes no storage.tsv and leaves storage.out.
func TestStorageNeedsTheWord(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "playlist", true)
	os.WriteFile(filepath.Join(d.dir, storageOut), []byte("\nlimit\t8000\n"), 0o644)
	d.tick()
	d.bg.Wait()
	if _, err := os.Stat(filepath.Join(d.dir, storageName)); err == nil {
		t.Fatal("storage.tsv written without the word")
	}
	if _, err := os.Stat(filepath.Join(d.dir, storageOut)); err != nil {
		t.Fatal("storage.out taken without the word")
	}
	if d.settings().LimitMB != 0 {
		t.Fatal("the limit changed without the word")
	}
}

// storage.tsv: the limit, the synced songs' bytes as downloads, the app's own folder as cache, and
// one picked item per chosen item with its bytes and where the firmware opens it.
func TestStorageTSVListsTheSyncedItems(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 2}, "storage", true)
	d.do("toggle", "album:al0")
	d.do("toggle", "library:all")
	d.do("toggle", "library:all") // on and off: the Entire Library row comes and goes
	d.preview(t)
	d.do("start")
	eventually(t, "the sync to finish", func() bool { return d.runner.Snapshot().State == "done" })
	d.do("toggle", "album:al1") // chosen, not synced yet: 0 bytes
	d.tick()
	d.bg.Wait()
	b, err := os.ReadFile(filepath.Join(d.dir, storageName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	var down int64
	for _, e := range d.manifest().Songs {
		down += e.Size
	}
	if down == 0 || !strings.HasPrefix(lines[0], "#Q2STORE/1\tlimit=0\tdownloads=") ||
		!strings.Contains(lines[0], "\tdownloads="+itoa(down)+"\tcache=") {
		t.Fatalf("header %q, downloads %d", lines[0], down)
	}
	if len(lines) != 3 || lines[1] != "i\tpick\talbum:al0\t"+itoa(down)+"\talbum\talbum:al0\tAlbum 000" ||
		!strings.HasPrefix(lines[2], "i\tpick\talbum:al1\t0\talbum\talbum:al1\t") {
		t.Fatalf("items %q", lines[1:])
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The Entire Library and a single song have no page of their own: the firmware opens On This Q2.
func TestStorageItemPages(t *testing.T) {
	for kind, want := range map[string]string{"album": "album", "playlist": "playlist", "artist": "artist", "song": "onq2", "library": "onq2"} {
		if op, _ := stItem(selection.Kind(kind)); op != want {
			t.Errorf("%s opens %q, want %q", kind, op, want)
		}
	}
}

// storage.out: a limit is saved (an unknown one is not), a remove takes the item out of Sync, and
// the file is gone once taken, so the firmware's next append starts a new one.
func TestStorageTakesTheFirmwaresRequests(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 2, songsPer: 1}, "storage", true)
	d.do("toggle", "album:al0")
	d.do("toggle", "album:al1")
	os.WriteFile(filepath.Join(d.dir, storageOut), []byte("\nlimit\t16000\n\nremove\talbum:al0\n\nlimit\t123\n"), 0o644)
	d.tick()
	d.bg.Wait()
	if got := d.settings().LimitMB; got != 16000 {
		t.Fatalf("limit %d", got)
	}
	sel, _ := d.selection()
	if sel.Has("album", "al0") || !sel.Has("album", "al1") {
		t.Fatalf("selection %+v", sel.Items)
	}
	for _, f := range []string{storageOut, storageOut + ".taking"} {
		if _, err := os.Stat(filepath.Join(d.dir, f)); err == nil {
			t.Fatalf("%s left", f)
		}
	}
	b, _ := os.ReadFile(filepath.Join(d.dir, storageName))
	if !strings.HasPrefix(string(b), "#Q2STORE/1\tlimit=16000\t") || strings.Contains(string(b), "album:al0") {
		t.Fatalf("storage.tsv %q", b)
	}
	// a file a crash left taken is applied at the next start
	os.WriteFile(filepath.Join(d.dir, storageOut+".taking"), []byte("limit\t0\n"), 0o644)
	if !d.stTake() || d.settings().LimitMB != 0 {
		t.Fatalf("the taken file was not applied: %d", d.settings().LimitMB)
	}
}

// A sync that would take the downloads past the limit shows by how much and offers no Start.
func TestPreviewOverTheLimit(t *testing.T) {
	if overLimit(0, 5e9, 1e9) != 0 || overLimit(8e9, 5e9, 1e9) != 0 || overLimit(8e9, 9e9, -1e9) != 0 {
		t.Fatal("within the limit, without one, or freeing space counts as over")
	}
	if got := overLimit(8e9, 7e9, 2e9); got != 1e9 {
		t.Fatalf("over %d", got)
	}
	p := &plan.Plan{NeedBytes: 2e9, FreeBytes: 50e9, Fits: true, Add: []plan.Want{{Bytes: 2e9}}}
	r := strings.Join(titles(previewRows(p, 0, 1e9, 8e9)), "|")
	if !strings.Contains(r, "Over the 8.0 GB Downloads limit · Take out 1.0 GB") || strings.Contains(r, "Start Sync") {
		t.Fatalf("rows %q", r)
	}
	if r := strings.Join(titles(previewRows(p, 0, 0, 8e9)), "|"); !strings.Contains(r, "Start Sync") {
		t.Fatalf("within the limit: %q", r)
	}

	d := newTestDaemon(t, &nav{albums: 1, songsPer: 2}, "storage", true)
	s := d.settings()
	s.LimitMB = 4000
	d.saveSettings(s)
	d.do("toggle", "album:al0")
	m := d.manifest() // already at the limit
	m.Songs["x"] = &library.Entry{ID: "x", Path: filepath.Join(d.card, "Subsonic", "x.flac"), Size: 4e9}
	os.MkdirAll(filepath.Join(d.card, "Subsonic"), 0o755)
	os.WriteFile(filepath.Join(d.card, "Subsonic", "x.flac"), nil, 0o644) // there, so the check keeps it
	if err := m.Save(d.card, filepath.Join(d.card, ".subsonic", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	r2 := d.preview(t)
	if _, ok := find(r2, "Over the 4.0 GB Downloads limit"); !ok {
		t.Fatalf("preview %q", titles(r2))
	}
	if m := d.do("start").Message; m != "Over the Downloads limit" {
		t.Fatalf("start: %q", m)
	}
}

func TestCapsStorage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "caps")
	os.WriteFile(p, []byte("playlist storage\n"), 0o644)
	if c := fw.ReadCaps(p); !c.Storage || c.Ratings {
		t.Fatalf("%+v", c)
	}
}
