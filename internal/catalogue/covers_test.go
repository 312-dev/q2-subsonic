package catalogue

import (
	"errors"
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
