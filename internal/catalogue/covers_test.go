package catalogue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/andsuor/q2-subsonic/internal/fakesonic"
)

// The albums that get covers are the catalogue's: one each, in the Library's order (by name, case
// folded), none for an album the app synced whole or one only a gone song had.
func TestCoverAlbums(t *testing.T) {
	lib := []fakesonic.Song{
		{ID: "s1", Album: "beta", AlbumID: "b", Suffix: "flac"},
		{ID: "s2", Album: "Alpha", AlbumID: "a", Suffix: "flac"},
		{ID: "s3", Album: "alpha", AlbumID: "a", Suffix: "flac"},
		{ID: "s4", Album: "Synced", AlbumID: "y", Suffix: "flac"},
		{ID: "s5", Album: "Slash", AlbumID: "x/..", Suffix: "flac"},
	}
	r := newRig(t, lib)
	r.walk(nil)
	got, err := Albums(r.st, Build{Account: acct, Synced: map[string]string{"s4": "/c/s4.flac"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Album{{"a", "Alpha", ""}, {"b", "beta", ""}}; !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

// Plan deletes what the catalogue no longer lists and leaves out what is there or noted as having
// none; the budget, counted in clusters, caps both the plan and the writes.
func TestCoverPlanAndBudget(t *testing.T) {
	card := t.TempDir()
	c := Covers{Card: card, Dir: filepath.Join(card, ".subsonic", CoversName)}
	os.MkdirAll(c.Dir, 0o755)
	os.WriteFile(c.Path("gone"), []byte("x"), 0o644)
	os.WriteFile(c.Path("have"), []byte("x"), 0o644)
	os.WriteFile(c.Path("half")+".tmp", []byte("x"), 0o644)
	albums := []Album{{ID: "have"}, {ID: "a"}, {ID: "none"}, {ID: "b"}, {ID: "c"}}
	p, err := c.Plan(albums)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Todo, []string{"a", "none", "b", "c"}) {
		t.Fatal(p.Todo)
	}
	if _, err := os.Stat(c.Path("gone")); !os.IsNotExist(err) {
		t.Fatal("a cover of an album gone from the catalogue is kept")
	}
	if _, err := os.Stat(c.Path("half") + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("a cut write is kept")
	}
	if err := p.None("none"); err != nil {
		t.Fatal(err)
	}
	unit, _ := c.cluster()
	c.Budget = 3 * unit // "have" and two more
	p, _ = c.Plan(albums)
	if !slices.Equal(p.Todo, []string{"a", "b"}) || p.Used() != unit {
		t.Fatal(p.Todo, p.Used())
	}
	if err := p.Save("a", []byte("jpeg")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(c.Path("a")); string(b) != "jpeg" {
		t.Fatal(string(b))
	}
	if err := p.Save("b", make([]byte, unit+1)); !errors.Is(err, ErrCardFull) { // two clusters: past the budget
		t.Fatal(err)
	}
	if err := p.Save("../x", []byte("j")); err == nil {
		t.Fatal("a path out of the folder")
	}
}

// A folder at its budget but for less than one of its covers plans nothing: a cover fetched then
// would only be refused by Save, round after round.
func TestCoverPlanCountsTheAverageCover(t *testing.T) {
	card := t.TempDir()
	c := Covers{Card: card, Dir: filepath.Join(card, ".subsonic", CoversName)}
	os.MkdirAll(c.Dir, 0o755)
	unit, _ := c.cluster()
	for _, id := range []string{"a", "b", "c"} {
		os.WriteFile(c.Path(id), make([]byte, 4*unit), 0o644)
	}
	c.Budget = 12*unit + 2*unit // the three, and room for half of one more
	albums := []Album{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	p, err := c.Plan(albums)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Todo) != 0 {
		t.Fatal("planned past the budget:", p.Todo)
	}
	c.Budget = 12*unit + 4*unit // room for one
	if p, _ = c.Plan(albums); !slices.Equal(p.Todo, []string{"d"}) {
		t.Fatal(p.Todo)
	}
}

// Open is a pass for one cover at a time: it counts the folder against the budget, deletes nothing,
// and Noted reads .none.
func TestCoverOpen(t *testing.T) {
	card := t.TempDir()
	c := Covers{Card: card, Dir: filepath.Join(card, ".subsonic", CoversName, BigName), Px: BigPx, Body: BigBody}
	p, err := c.Open() // makes the folder
	if err != nil || p.Used() != 0 {
		t.Fatal(p, err)
	}
	os.WriteFile(c.Path("other"), []byte("x"), 0o644)
	unit, _ := c.cluster()
	c.Budget = 2 * unit
	p, _ = c.Open()
	if p.Used() != unit {
		t.Fatal(p.Used())
	}
	if err := p.Save("a", []byte("jpeg")); err != nil {
		t.Fatal(err)
	}
	if err := p.Save("b", []byte("jpeg")); !errors.Is(err, ErrCardFull) {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Path("other")); err != nil {
		t.Fatal("Open deleted a cover:", err)
	}
	if c.Noted("n") {
		t.Fatal("noted before None")
	}
	p.None("n")
	if !c.Noted("n") {
		t.Fatal("not noted after None")
	}
	if _, err := (Covers{Card: card, Dir: filepath.Join(t.TempDir(), "covers")}).Open(); err == nil {
		t.Fatal("a folder outside the card")
	}
}

// The owner's card (2026-11-24): exFAT with 128 KiB clusters, 119 GB with 116 GB free, and a catalogue of 6,174
// albums. Each cover takes a whole cluster, so a fixed 256 MiB budget held 2,048 of them and planned nothing
// for the other 4,126, round after round. The budget is room for every album planned.
func TestCoverPlanHasRoomForEveryAlbumOnACardOfBigClusters(t *testing.T) {
	card := t.TempDir()
	const unit = 128 << 10
	space := func() (int64, int64, int64) { return unit, 116 << 30, 119 << 30 }
	albums := make([]Album, 6174)
	for i := range albums {
		albums[i] = Album{ID: fmt.Sprintf("al%d", i)}
	}
	for _, c := range []Covers{
		{Card: card, Dir: filepath.Join(card, ".subsonic", CoversName), Space: space},
		{Card: card, Dir: filepath.Join(card, ".subsonic", CoversName, BigName), Px: BigPx, Body: BigBody, Space: space},
	} {
		p, err := c.Plan(albums)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Todo) != len(albums) || p.Stopped != 0 {
			t.Fatalf("%s: %d of %d albums planned, %d left out by a budget of %d MiB", c.Dir, len(p.Todo), len(albums),
				p.Stopped, p.Budget>>20)
		}
		if p.Budget > (119<<30)/CoverShare {
			t.Fatalf("budget %d MiB past the card's share", p.Budget>>20)
		}
		// and a cover saved at the end of the plan fits
		room := p.Budget - int64(len(albums)-1)*unit
		if room < unit {
			t.Fatalf("budget %d MiB has no room for the last album", p.Budget>>20)
		}
	}
}

// A small card keeps a bound: its covers' budget is at most its size over CoverShare (but never under
// CoverBudget), the plan says how many albums that leaves out, and a nearly full card writes none (CoverFloor).
func TestCoverBudgetOnASmallOrNearlyFullCard(t *testing.T) {
	card := t.TempDir()
	const unit = 128 << 10
	albums := make([]Album, 6174)
	for i := range albums {
		albums[i] = Album{ID: fmt.Sprintf("al%d", i)}
	}
	small := Covers{Card: card, Dir: filepath.Join(card, ".subsonic", CoversName),
		Space: func() (int64, int64, int64) { return unit, 6 << 30, 8 << 30 }}
	p, err := small.Plan(albums)
	if err != nil {
		t.Fatal(err)
	}
	if p.Budget != CoverBudget || len(p.Todo) != CoverBudget/unit || p.Stopped != len(albums)-CoverBudget/unit {
		t.Fatalf("an 8 GB card: budget %d MiB, %d planned, %d left out", p.Budget>>20, len(p.Todo), p.Stopped)
	}
	mid := small
	mid.Space = func() (int64, int64, int64) { return unit, 10 << 30, 16 << 30 }
	if p, _ = mid.Plan(albums); p.Budget != (16<<30)/CoverShare || p.Stopped != len(albums)-int(p.Budget/unit) {
		t.Fatalf("a 16 GB card: budget %d MiB, %d left out", p.Budget>>20, p.Stopped)
	}
	full := small
	full.Space = func() (int64, int64, int64) { return unit, CoverFloor + unit/2, 119 << 30 }
	if p, _ = full.Plan(albums); len(p.Todo) != len(albums) {
		t.Fatalf("%d planned", len(p.Todo))
	}
	if err := p.Save("al0", []byte("jpeg")); !errors.Is(err, ErrCardFull) {
		t.Fatalf("a cover written with the card at its floor: %v", err)
	}
}
