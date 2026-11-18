package catalogue

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/andsuor/q2-subsonic/internal/library"
)

// Covers is the albums' covers the firmware shows beside the catalogue's server rows (q2-coda's
// docs/server-library.md, "Covers"): <app folder>/covers/<album id>.jpg, a baseline JPEG of at most
// CoverPx a side, one for each album the catalogue has an s row of. The firmware reads one wherever
// it would read a card album's cover (Library's album rows and grids, Cover Flow, Now Playing), by
// the album id in a server row's path, and shows the placeholder while there is none; so a cover is
// fetched whenever there is time, and the catalogue never waits for one.
//
// The folder is bounded: its files, each counted in whole clusters of the card, stay within
// Budget, and none is written while the card has less than CoverFloor free. An album the server has
// no cover for is noted in covers/.none and not asked again until Sign Out; a cover whose album
// leaves the catalogue is deleted (Plan). A playlist's cover is kept with the coverArt id it was
// fetched by (covers/.art), as Navidrome names each new collage anew: a new id fetches it again,
// and a playlist noted without one is asked again under its new id.
//
// The same albums' covers at BigPx are a second Covers, Dir covers/512 (BigName), with its own
// budget and notes: the car remote sends them when the phone asks for more than CoverPx, as the
// 160 px ones look soft on a car's screen. They are fetched once the 160 px set has nothing left
// to fetch, and the Q2 itself never draws them (its rows decode a cover whole on its UI thread).
//
// A cover the car asks for before a round reaches it is fetched at once and saved through Open, in
// either folder, within the same budget and notes.
type Covers struct {
	Card, Dir string
	Budget    int64 // 0: CoverBudget
	Px, Body  int   // a cover's side and the server's bytes at most; 0: CoverPx, CoverBody
}

// Side is the largest a cover of c is, either way.
func (c Covers) Side() int {
	if c.Px > 0 {
		return c.Px
	}
	return CoverPx
}

// MaxBody is the most bytes the server may send for one of c's covers.
func (c Covers) MaxBody() int {
	if c.Body > 0 {
		return c.Body
	}
	return CoverBody
}

const (
	CoversName  = "covers"
	CoverPx     = 160        // Cover Flow's texture side (q2-coda patch/coverflow.h ART_SIZE); rows and grids are smaller
	CoverBody   = 64 << 10   // a cover's bytes at most as the server sends it
	CoverBudget = 256 << 20  // of the card for the folder, in clusters
	CoverFloor  = 1024 << 20 // the card's free space below which no cover is written
	noneName    = ".none"
	artName     = ".art" // "<file id> <coverArt id>" a line, for each cover fetched by an Art id

	BigName = "512"     // covers/512/: the same covers at BigPx, for the car (Q2 Muse's coda-rc)
	BigPx   = 512       // a cover's side at most there
	BigBody = 256 << 10 // a 512 px cover's bytes at most as the server sends it
)

// Album is one album of the catalogue, or a playlist's cover (PlaylistCovers): ID names its file,
// Art, when not "", is the coverArt id the server is asked for in its place.
type Album struct{ ID, Name, Art string }

// PlaylistCovers is each playlist with a cover as an Album named pl-<playlist id>, the file the
// firmware's Playlists rows look for (q2-coda's docs/server-library.md, Covers), asked for by the
// playlist's coverArt id.
func PlaylistCovers(pls []Playlist) []Album {
	var out []Album
	for _, p := range pls {
		if id := "pl-" + p.ID; p.Art != "" && CoverID(p.ID) {
			out = append(out, Album{id, p.Name, p.Art})
		}
	}
	return out
}

// Albums is the albums the catalogue lists s rows of (Write's songs), each once, in the order the
// Library's Albums list shows them (by name, case folded): the top of the list gets its covers
// first.
func Albums(st Store, b Build) ([]Album, error) {
	seen := map[string]bool{}
	var out []Album
	err := st.Each(b.Account, func(s Song) error {
		if _, synced := b.Synced[s.ID]; synced || s.Gone && b.Local[s.ID] == "" || !CoverID(s.AlbumID) || seen[s.AlbumID] {
			return nil
		}
		seen[s.AlbumID] = true
		out = append(out, Album{s.AlbumID, s.Album, ""})
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, err
}

// CoverID is whether id can name a file: the firmware builds the path from it as it is.
func CoverID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}

func (c Covers) budget() int64 {
	if c.Budget > 0 {
		return c.Budget
	}
	return CoverBudget
}

// Path is album id's cover file.
func (c Covers) Path(id string) string { return filepath.Join(c.Dir, id+".jpg") }

// cluster is the card's allocation unit and its free bytes; 4 KiB and plenty when it can't be read.
func (c Covers) cluster() (int64, int64) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(c.Dir, &fs); err != nil || fs.Bsize <= 0 {
		return 4096, 1 << 62
	}
	return int64(fs.Bsize), int64(fs.Bavail) * int64(fs.Bsize)
}

func clusters(size, unit int64) int64 { return (size + unit - 1) / unit * unit }

// CoverPass is one round of fetching: the albums to fetch, in order, and the folder's size so far.
type CoverPass struct {
	Todo       []string
	c          Covers
	used, unit int64
	art, saved map[string]string // the coverArt id each file is asked by; and was fetched by (.art)
}

// Plan deletes the covers of albums no longer in albums (and any file that is not a cover), and
// returns the albums still to fetch, in albums' order: those with no file and no note in .none,
// as many as the budget has room for beside what is there, each counted at the size of the
// folder's average cover (one cluster while it has none). A cover that won't fit is never
// fetched, only to be refused by Save. It makes the folder.
func (c Covers) Plan(albums []Album) (*CoverPass, error) {
	if !library.Owned(c.Card, c.Path("x"), nil) {
		return nil, errors.New("refusing covers outside owned trees: " + c.Dir)
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, a := range albums {
		want[a.ID] = true
	}
	none, arts := c.none(), c.arts()
	unit, _ := c.cluster()
	var used, n int64
	have := map[string]bool{}
	ents, err := os.ReadDir(c.Dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		id, isCover := strings.CutSuffix(name, ".jpg")
		switch {
		case name == noneName, name == artName, name == BigName && e.IsDir(): // covers/512: a Covers of its own
		case isCover && want[id] && e.Type().IsRegular():
			if info, err := e.Info(); err == nil {
				used += clusters(info.Size(), unit)
				n++
			}
			have[id] = true
		default: // an album gone from the catalogue, a .tmp left by a cut write
			os.RemoveAll(filepath.Join(c.Dir, name))
		}
	}
	p := &CoverPass{c: c, used: used, unit: unit, art: map[string]string{}, saved: map[string]string{}}
	room, each := used, unit // a cover to come takes what the folder's average one does
	if n > 0 {
		each = max(unit, clusters(used/n, unit))
	}
	for _, a := range albums {
		if a.Art != "" {
			p.art[a.ID] = a.Art
			if have[a.ID] && arts[a.ID] != "" {
				p.saved[a.ID] = arts[a.ID]
			}
		}
		if have[a.ID] && arts[a.ID] == a.Art || none[noteKey(a.ID, a.Art)] {
			continue
		}
		if room+each > c.budget() {
			break
		}
		room += each
		p.Todo = append(p.Todo, a.ID)
	}
	return p, nil
}

// Open is a CoverPass for album covers saved one at a time between rounds (the car's asks,
// q2-subsonic's coverask.go): the folder's size so far, nothing to fetch and nothing deleted. It
// makes the folder.
func (c Covers) Open() (*CoverPass, error) {
	if !library.Owned(c.Card, c.Path("x"), nil) {
		return nil, errors.New("refusing covers outside owned trees: " + c.Dir)
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return nil, err
	}
	unit, _ := c.cluster()
	ents, err := os.ReadDir(c.Dir)
	if err != nil {
		return nil, err
	}
	var used int64
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".jpg") && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				used += clusters(info.Size(), unit)
			}
		}
	}
	return &CoverPass{c: c, used: used, unit: unit, art: map[string]string{}, saved: map[string]string{}}, nil
}

// Noted is whether album id is noted in .none as having no cover.
func (c Covers) Noted(id string) bool { return c.none()[id] }

// noteKey is how .none notes id: with the coverArt id it was asked by, when it has one.
func noteKey(id, art string) string {
	if art != "" {
		return id + " " + art
	}
	return id
}

// arts is .art: each cover fetched by a coverArt id, and that id.
func (c Covers) arts() map[string]string {
	out := map[string]string{}
	lines(filepath.Join(c.Dir, artName), nil, func(l string) error { return addArt(out, l) })
	return out
}

func addArt(m map[string]string, line string) error {
	if id, art, ok := strings.Cut(strings.TrimSpace(line), " "); ok && id != "" && art != "" {
		m[id] = art
	}
	return nil
}

// none is the albums noted as having no cover.
func (c Covers) none() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(filepath.Join(c.Dir, noneName))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id := strings.TrimSpace(sc.Text()); id != "" {
			out[id] = true
		}
	}
	return out
}

// ErrCardFull is a cover not written: the folder is at its budget or the card at CoverFloor.
var ErrCardFull = errors.New("covers: no room on the card")

// Save writes album id's cover, b (a baseline JPEG), whole and renamed into place, unless that
// would pass the budget or leave the card under CoverFloor.
func (p *CoverPass) Save(id string, b []byte) error {
	if !CoverID(id) {
		return errors.New("covers: bad album id " + id)
	}
	size := clusters(int64(len(b)), p.unit)
	if _, free := p.c.cluster(); free-size < CoverFloor || p.used+size > p.c.budget() {
		return ErrCardFull
	}
	f, err := create(p.c.Card, p.c.Path(id))
	if err != nil {
		return err
	}
	if _, err := f.WriteString(string(b)); err != nil {
		f.abort()
		return err
	}
	if err := f.commit(); err != nil {
		return err
	}
	p.used += size
	if art := p.art[id]; art != "" {
		p.saved[id] = art
		return p.writeArts()
	}
	return nil
}

// writeArts writes .art whole: the covers of this pass's albums with their coverArt ids.
func (p *CoverPass) writeArts() error {
	f, err := create(p.c.Card, filepath.Join(p.c.Dir, artName))
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(p.saved))
	for id := range p.saved {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f.WriteString(id + " " + p.saved[id] + "\n")
	}
	return f.commit()
}

// Used is the folder's size in the card's clusters.
func (p *CoverPass) Used() int64 { return p.used }

// None notes that the server has no cover for album id (a playlist's: under its coverArt id).
func (p *CoverPass) None(id string) error {
	c := p.c
	if !CoverID(id) {
		return nil
	}
	path := filepath.Join(c.Dir, noneName)
	if !library.Owned(c.Card, path, nil) {
		return errors.New("refusing path outside owned trees: " + path)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(noteKey(id, p.art[id]) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// RemoveCovers deletes the folder, as at Sign Out or when Show in Library is turned off.
func RemoveCovers(card, dir string) {
	path := filepath.Join(dir, CoversName)
	if library.Owned(card, path, nil) {
		os.RemoveAll(path)
	}
}
