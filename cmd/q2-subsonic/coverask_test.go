package main

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/catalogue"
	"github.com/andsuor/q2-subsonic/internal/fakesonic"
)

// newCoverRig is a catRig whose catalogue is written (al0 to al2), each album's cover on the server
// at 160 and 512 px, and none fetched yet.
func newCoverRig(t *testing.T) *catRig {
	t.Helper()
	r := newCatRig(t, "catalogue", fakesonic.Library(3))
	r.f.Mu.Lock()
	r.f.Covers = map[string][]byte{}
	for i := range 3 {
		r.f.Covers[fmt.Sprintf("al%d@160", i)] = testJPEG(t, 160)
		r.f.Covers[fmt.Sprintf("al%d@512", i)] = testJPEG(t, 512)
	}
	r.f.Mu.Unlock()
	r.check(t) // the first copy and the catalogue: no cover is fetched in that check
	if n := r.calls("getCoverArt"); n != 0 {
		t.Fatal("covers in the catalogue's first check:", n)
	}
	return r
}

// askCover writes the firmware's .coverask request, lines "<seq> <px> <album id>", and has the app
// read it, as its tick does.
func (r *catRig) askCover(lines ...string) {
	os.WriteFile(filepath.Join(r.tmp, "q2-subsonic.coverask"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	r.coverTick()
}

func (r *catRig) stateKey(key string) string {
	for _, l := range strings.Split(r.state(), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func (r *catRig) answered(t *testing.T, want string) {
	t.Helper()
	eventually(t, "coverack="+want, func() bool { return r.stateKey("coverack") == want })
	r.bg.Wait()
}

func coverWidth(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return -1
	}
	return cfg.Width
}

// The car asks for a cover the Q2 hasn't: the app fetches it at once at the size asked, saves it
// where the firmware looks (covers/512/ for the car's size, covers/ for the Q2's) and answers; a
// second ask finds the file and asks the server nothing.
func TestCoverAskFetchesOnce(t *testing.T) {
	r := newCoverRig(t)
	dir := r.file(catalogue.CoversName)
	r.askCover("5 512 al0")
	r.answered(t, "5 0")
	if w := coverWidth(filepath.Join(dir, catalogue.BigName, "al0.jpg")); w != 512 {
		t.Fatal("al0's 512 px cover:", w)
	}
	if w := coverWidth(filepath.Join(dir, "al0.jpg")); w != 0 {
		t.Fatal("a 160 px cover the car didn't ask for:", w)
	}
	r.askCover("6 512 al0")
	r.answered(t, "6 0")
	r.askCover("7 160 al1")
	r.answered(t, "7 0")
	if w := coverWidth(filepath.Join(dir, "al1.jpg")); w != 160 {
		t.Fatal("al1's 160 px cover:", w)
	}
	if n := r.calls("getCoverArt"); n != 2 {
		t.Fatal("getCoverArt calls:", n)
	}
}

// One request file can hold several covers: each line is answered in order, and a line read before
// is not asked again when the file is written with a new one beside it.
func TestCoverAskSeveral(t *testing.T) {
	r := newCoverRig(t)
	r.askCover("1 512 al0", "2 512 al1")
	r.answered(t, "2 0")
	r.askCover("1 512 al0", "2 512 al1", "3 512 al2")
	r.answered(t, "3 0")
	for i := range 3 {
		if w := coverWidth(filepath.Join(r.bigCovers().Dir, fmt.Sprintf("al%d.jpg", i))); w != 512 {
			t.Fatalf("al%d: %d", i, w)
		}
	}
	if n := r.calls("getCoverArt"); n != 3 {
		t.Fatal("getCoverArt calls:", n)
	}
}

// A cover the server has none of is noted, as a round notes it, and never asked again; the round
// doesn't ask for it either.
func TestCoverAskNone(t *testing.T) {
	r := newCoverRig(t)
	r.f.Mu.Lock()
	delete(r.f.Covers, "al1@512")
	r.f.Mu.Unlock()
	r.askCover("3 512 al1")
	r.answered(t, "3 2")
	r.askCover("4 512 al1")
	r.answered(t, "4 2")
	if n := r.calls("getCoverArt"); n != 1 {
		t.Fatal("getCoverArt calls:", n)
	}
	if got := catLines(t, filepath.Join(r.bigCovers().Dir, ".none")); !slices.Equal(got, []string{"al1"}) {
		t.Fatal(".none:", got)
	}
}

// A server out of reach is a failure, remembered for coverFailFor: asks meanwhile are answered at
// once without the server; after it, the cover is fetched.
func TestCoverAskFailureRemembered(t *testing.T) {
	r := newCoverRig(t)
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	r.askCover("1 512 al0")
	r.answered(t, "1 3")
	tried := r.calls("getCoverArt")
	if tried == 0 {
		t.Fatal("the server was not asked")
	}
	r.f.Mu.Lock()
	r.f.Down = false
	r.f.Mu.Unlock()
	r.askCover("2 512 al0")
	r.answered(t, "2 3")
	if n := r.calls("getCoverArt"); n != tried {
		t.Fatal("asked again within coverFailFor:", n-tried)
	}
	r.cov.mu.Lock()
	for k := range r.cov.failed {
		r.cov.failed[k] = time.Now().Add(-coverFailFor - time.Second)
	}
	r.cov.mu.Unlock()
	r.askCover("3 512 al0")
	r.answered(t, "3 0")
	if n := r.calls("getCoverArt"); n != tried+1 {
		t.Fatal("getCoverArt calls after coverFailFor:", n-tried)
	}
	if w := coverWidth(filepath.Join(r.bigCovers().Dir, "al0.jpg")); w != 512 {
		t.Fatal("al0:", w)
	}
}

// What isn't a cover of the app's to fetch is answered at once, with no request: a name that could
// leave the folder, a size it keeps none at, and anything once signed out.
func TestCoverAskNotOurs(t *testing.T) {
	r := newCoverRig(t)
	r.askCover("1 512 ../al0")
	r.answered(t, "1 1")
	r.askCover("2 300 al0")
	r.answered(t, "2 1")
	r.askCover("3 512")
	r.askCover("4 512 al0 x")
	r.answered(t, "4 1")
	r.signOut()
	r.bg.Wait()
	r.askCover("5 512 al0")
	r.answered(t, "5 1")
	if n := r.calls("getCoverArt"); n != 0 {
		t.Fatal("getCoverArt calls:", n)
	}
}

// A cover saved for the car leaves the minute's rounds settled when they were: the folder's new
// time would otherwise have the next round read the store and every cover again.
func TestCoverAskKeepsTheRoundsSettled(t *testing.T) {
	r := newCoverRig(t)
	r.check(t) // the Q2's covers
	r.check(t) // the car's
	r.check(t) // nothing left: settled
	planned := func() int {
		r.sl.mu.Lock()
		defer r.sl.mu.Unlock()
		return r.sl.planned
	}
	os.Remove(filepath.Join(r.bigCovers().Dir, "al2.jpg"))
	r.check(t) // the folder changed: one round, which fetches al2 again and settles
	before, calls := planned(), r.calls("getCoverArt")
	os.Remove(filepath.Join(r.bigCovers().Dir, "al1.jpg"))
	r.slSettle() // as a round that found the folder so would have
	r.askCover("9 512 al1")
	r.answered(t, "9 0")
	for range 3 {
		r.check(t)
	}
	if n := planned() - before; n != 0 {
		t.Fatalf("%d rounds read the store after the car's cover", n)
	}
	if n := r.calls("getCoverArt") - calls; n != 1 {
		t.Fatal("getCoverArt calls:", n)
	}
}
