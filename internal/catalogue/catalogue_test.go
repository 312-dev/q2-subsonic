package catalogue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/fakesonic"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

const acct = "0badcafe"

type rig struct {
	t    *testing.T
	card string
	dir  string
	f    *fakesonic.Server
	cl   *subsonic.Client
	st   Store
}

func newRig(t *testing.T, songs []fakesonic.Song) *rig {
	t.Helper()
	card := t.TempDir()
	dir := filepath.Join(card, ".subsonic")
	os.MkdirAll(dir, 0o755)
	f := fakesonic.New()
	t.Cleanup(f.Close)
	f.Songs, f.Count = songs, int64(len(songs))
	cl, err := subsonic.New(subsonic.Creds{Server: f.URL, User: "u", Password: "pw"}, f.Client())
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t, card, dir, f, cl, Store{card, filepath.Join(dir, SongsName)}}
}

func (r *rig) walk(sent map[string]int) WalkResult {
	r.t.Helper()
	r.f.Mu.Lock()
	hint := Hint{Count: r.f.Count}
	r.f.Mu.Unlock()
	res, err := r.st.Walk(context.Background(), r.cl, acct, hint, sent, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *rig) songs() map[string]Song {
	out := map[string]Song{}
	r.st.Each(acct, func(s Song) error { out[s.ID] = s; return nil })
	return out
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// The first copy pages the whole library 500 at a time and keeps every field the catalogue needs.
func TestWalkPagesTheLibrary(t *testing.T) {
	lib := fakesonic.Library(1203)
	lib[0].Genre, lib[0].MBID = "Rock", "mb-1"
	r := newRig(t, lib)
	res := r.walk(nil)
	if res.Songs != 1203 || res.Requests != 3 || !res.Complete || res.Gone != 0 {
		t.Fatalf("%+v", res)
	}
	if n := r.f.Calls["search3"]; n != 3 {
		t.Fatal("search3 calls", n)
	}
	s := r.songs()["s1"]
	if len(r.songs()) != 1203 || s.Genre != "Rock" || s.MBID != "mb-1" || s.Size != 1000000 || s.Seconds != 180 || s.AlbumID != "al0" {
		t.Fatalf("%+v", s)
	}
	if _, err := os.Stat(r.st.Path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp file left")
	}
}

// catalogue.tsv: the header's rows count every line, a synced song has no row, a cached one names
// its copy, and size and MusicBrainz id close each s row.
func TestWriteCatalogue(t *testing.T) {
	lib := fakesonic.Library(5)
	lib[1].Title = "Tab\there"
	lib[2].MBID = "mb-3"
	r := newRig(t, lib)
	r.walk(nil)
	pls := []Playlist{{ID: "p1", Name: "Mix", SIDs: []string{"s2", "s1", "s9", "s2"}}}
	cat := filepath.Join(r.dir, CatName)
	os.WriteFile(filepath.Join(r.dir, AvailName), []byte("s4\t/x\n"), 0o644)
	b := Build{Card: r.card, App: "subsonic", Account: acct,
		Synced: map[string]string{"s1": r.card + "/Subsonic/A/01 - s1.flac"},
		Local:  map[string]string{"s3": r.dir + "/stream/c/original/0/t [s3].flac"}}
	seq, rows, err := Write(cat, r.st, pls, b)
	if err != nil {
		t.Fatal(err)
	}
	l := readLines(t, cat)
	if seq != 1 || rows != len(l)-1 || l[0] != "#Q2CAT/1\tseq=1\taccount=0badcafe\trows=8" {
		t.Fatalf("seq %d rows %d: %q", seq, rows, l[0])
	}
	want := []string{
		"s\ts2\tTab%09here\tArtist\tArtist\tAlbum 1\tal1\t2000\t1\t1\t181\t\tflac\t\t1000001\t",
		"s\ts3\tTitle s3\tArtist\tArtist\tAlbum 2\tal2\t2000\t1\t1\t182\t\tflac\t" + b.Local["s3"] + "\t1000002\tmb-3",
		"s\ts4\tTitle s4\tArtist\tArtist\tAlbum 3\tal3\t2000\t1\t1\t183\t\tflac\t\t1000003\t",
		"s\ts5\tTitle s5\tArtist\tArtist\tAlbum 4\tal4\t2000\t1\t1\t184\t\tflac\t\t1000004\t",
		"p\tp1\tMix",
		"i\tp1\ts2\t",
		"i\tp1\ts1\t" + b.Synced["s1"],
		"i\tp1\ts2\t",
	}
	if !slices.Equal(l[1:], want) {
		t.Fatalf("got\n%s", strings.Join(l[1:], "\n"))
	}
	if _, err := os.Stat(filepath.Join(r.dir, AvailName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("catalogue.avail not emptied")
	}
	if seq, _, _ := Write(cat, r.st, pls, b); seq != 2 {
		t.Fatal("seq", seq)
	}
}

// A song gone from the server is marked, kept in the store, and in the catalogue only while the
// cache has a copy; it comes back when the server has it again.
func TestGoneSongs(t *testing.T) {
	r := newRig(t, fakesonic.Library(10))
	r.walk(nil)
	r.f.Mu.Lock()
	s3 := r.f.Songs[2]
	r.f.Songs = slices.Delete(r.f.Songs, 2, 3)
	r.f.Count = 9
	r.f.Mu.Unlock()
	res := r.walk(nil)
	if res.Gone != 1 || !r.songs()["s3"].Gone || len(r.songs()) != 10 {
		t.Fatalf("%+v %+v", res, r.songs()["s3"])
	}
	cat := filepath.Join(r.dir, CatName)
	b := Build{Card: r.card, App: "subsonic", Account: acct}
	if _, rows, _ := Write(cat, r.st, nil, b); rows != 9 {
		t.Fatal("rows", rows)
	}
	b.Local = map[string]string{"s3": "/mnt/mmc/.subsonic/stream/x [s3].flac"}
	if _, rows, _ := Write(cat, r.st, nil, b); rows != 10 {
		t.Fatal("rows with the copy", rows)
	}
	r.f.Mu.Lock()
	r.f.Songs, r.f.Count = append(r.f.Songs, s3), 10
	r.f.Mu.Unlock()
	if res := r.walk(nil); res.Gone != 0 || r.songs()["s3"].Gone {
		t.Fatalf("back: %+v", res)
	}
}

// A walk short of the server's count marks nothing gone, and one that finds nothing at all keeps
// the store as it was.
func TestIncompleteWalk(t *testing.T) {
	r := newRig(t, fakesonic.Library(10))
	r.walk(nil)
	r.f.Mu.Lock()
	r.f.Songs = r.f.Songs[:8] // the count still says 10
	r.f.Mu.Unlock()
	if res := r.walk(nil); res.Complete || res.Gone != 0 || r.songs()["s10"].Gone {
		t.Fatalf("%+v", res)
	}
	r.f.Mu.Lock()
	r.f.NoEmpty = true
	r.f.Mu.Unlock()
	_, err := r.st.Walk(context.Background(), r.cl, acct, Hint{}, nil, nil)
	if !errors.Is(err, ErrNoSongs) || len(r.songs()) != 10 {
		t.Fatal(err, len(r.songs()))
	}
}

// A walk that fails keeps the store it had.
func TestFailedWalkKeepsStore(t *testing.T) {
	r := newRig(t, fakesonic.Library(600))
	r.walk(nil)
	before, _ := os.ReadFile(r.st.Path)
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	_, err := r.st.Walk(context.Background(), r.cl, acct, Hint{}, nil, nil)
	if !subsonic.ConnectionLost(err) {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(r.st.Path); string(after) != string(before) {
		t.Fatal("store changed")
	}
}

// Hearts: the first walk brings every rated or starred song; later ones only what changed, and
// not what the Q2 just sent.
func TestWalkHeartChanges(t *testing.T) {
	now = func() time.Time { return time.Unix(1000, 0) }
	t.Cleanup(func() { now = time.Now })
	lib := fakesonic.Library(5)
	lib[0].Rating = 3
	lib[1].Starred = "2026-11-01T00:00:00Z"
	r := newRig(t, lib)
	res := r.walk(nil)
	if len(res.Changes) != 2 || res.Changes[0].SID != "s1" || res.Changes[0].Hearts != 3 || res.Changes[0].At != 1000 ||
		res.Changes[1].Hearts != 5 || res.Changes[1].At != time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("%+v", res.Changes)
	}
	if res := r.walk(nil); len(res.Changes) != 0 {
		t.Fatalf("again: %+v", res.Changes)
	}
	r.f.Mu.Lock()
	r.f.Songs[0].Rating = 0
	r.f.Songs[2].Rating = 4
	r.f.Mu.Unlock()
	res = r.walk(map[string]int{"s3": 4})
	if len(res.Changes) != 1 || res.Changes[0].SID != "s1" || res.Changes[0].Hearts != 0 {
		t.Fatalf("%+v", res.Changes)
	}
}

func TestStarChanges(t *testing.T) {
	r := newRig(t, fakesonic.Library(3))
	r.walk(nil)
	starred := []subsonic.SongInfo{{Song: subsonic.Song{ID: "s2"}, Starred: "2026-11-02T00:00:00Z"}}
	ch, err := r.st.StarChanges(acct, starred, nil)
	if err != nil || len(ch) != 1 || ch[0].SID != "s2" || ch[0].Hearts != 5 || r.songs()["s2"].Starred == "" {
		t.Fatal(ch, err)
	}
	if ch, _ := r.st.StarChanges(acct, starred, nil); len(ch) != 0 {
		t.Fatal("again", ch)
	}
	if ch, _ := r.st.StarChanges(acct, nil, nil); len(ch) != 1 || ch[0].Hearts != 0 || r.songs()["s2"].Starred != "" {
		t.Fatal("unstarred", ch)
	}
}

// Playlists are asked for again only when their changed date moved.
func TestRefreshPlaylists(t *testing.T) {
	r := newRig(t, fakesonic.Library(3))
	r.f.Playlists = []fakesonic.Playlist{{ID: "p1", Name: "A", Changed: "t1", SIDs: []string{"s1", "s2"}},
		{ID: "p2", Name: "B", Changed: "t1", SIDs: []string{"s3"}}}
	ctx := context.Background()
	pls, changed, err := Refresh(ctx, r.cl, nil)
	if err != nil || !changed || len(pls) != 2 || !slices.Equal(pls[0].SIDs, []string{"s1", "s2"}) {
		t.Fatal(pls, changed, err)
	}
	if pls, changed, _ = Refresh(ctx, r.cl, pls); changed || r.f.Calls["getPlaylist"] != 2 {
		t.Fatal("unchanged refetched", changed, r.f.Calls)
	}
	r.f.Mu.Lock()
	r.f.Playlists[1].Changed, r.f.Playlists[1].SIDs = "t2", []string{"s3", "s1"}
	r.f.Mu.Unlock()
	pls, changed, _ = Refresh(ctx, r.cl, pls)
	if !changed || r.f.Calls["getPlaylist"] != 3 || !slices.Equal(pls[1].SIDs, []string{"s3", "s1"}) {
		t.Fatal(pls, changed, r.f.Calls)
	}
	r.f.Mu.Lock()
	r.f.Playlists[1].Changed = "t3" // a smart playlist asked for again: a new date, the same songs
	r.f.Mu.Unlock()
	old := pls
	if pls, changed, _ = Refresh(ctx, r.cl, pls); changed || SameDates(old, pls) || pls[1].Changed != "t3" {
		t.Fatal("a date alone", changed, pls)
	}
	ps := Playlists{r.card, filepath.Join(r.dir, PlaylistsName)}
	if err := ps.Save(acct, pls); err != nil {
		t.Fatal(err)
	}
	if back, ok := ps.Load(acct); !ok || len(back) != 2 || back[1].Changed != "t3" || !slices.Equal(back[1].SIDs, pls[1].SIDs) {
		t.Fatal(back, ok)
	}
	if _, ok := ps.Load("ffffffff"); ok {
		t.Fatal("another account's playlists")
	}
}

// listen_key is the firmware's: FNV-1a 32 of the path, one more multiply.
func TestListenKey(t *testing.T) {
	if k := ListenKey("/mnt/mmc/Subsonic/A/01 - B.flac"); k != "97753645" {
		t.Fatal(k)
	}
}

func TestFileURL(t *testing.T) {
	u := FileURL("/mnt/mmc", "subsonic", "al1", "s7", "flac")
	if u != "/mnt/mmc/.subsonic/cat/al1/s7.flac" || SIDOf("/mnt/mmc", "subsonic", u) != "s7" ||
		SIDOf("/mnt/mmc", "subsonic", "/mnt/mmc/Music/s7.flac") != "" {
		t.Fatal(u)
	}
}

// ratings.out: sent with setRating, five hearts starred, fewer unstarred; a newer server star wins;
// a lost server keeps what wasn't sent.
func TestSendRatings(t *testing.T) {
	lib := fakesonic.Library(5)
	lib[1].Starred = "2026-11-01T00:00:00Z" // s2
	lib[3].Starred = "2026-11-10T00:00:00Z" // s4, after the Q2's change below
	r := newRig(t, lib)
	day := func(d int) int64 { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC).Unix() }
	synced := ListenKey("/mnt/mmc/Subsonic/x.flac")
	outs := []Out{
		{"k1", "s1", 2, day(5)},
		{"k1", "s1", 5, day(6)}, // the last of s1's wins
		{"k2", "s2", 3, day(5)},
		{synced, "", 4, day(5)}, // a synced card file: s3 by its listen_key
		{"k4", "s4", 1, day(5)},
		{"kx", "", 2, day(5)}, // nobody's
	}
	sent, err := Send(context.Background(), r.cl, outs, func(k string) string {
		if k == synced {
			return "s3"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"setRating s1 5", "star s1", "setRating s2 3", "unstar s2", "setRating s3 4"}
	if !slices.Equal(r.f.Writes, want) {
		t.Fatal(r.f.Writes)
	}
	if sent.Dropped != 1 || len(sent.Rest) != 0 || sent.Hearts["s1"] != 5 || sent.Hearts["s3"] != 4 ||
		len(sent.Wins) != 1 || sent.Wins[0].SID != "s4" || sent.Wins[0].Hearts != 5 {
		t.Fatalf("%+v", sent)
	}
	r.f.Mu.Lock()
	r.f.Down = true
	r.f.Mu.Unlock()
	sent, err = Send(context.Background(), r.cl, []Out{{"k5", "s5", 2, day(5)}}, nil)
	if !subsonic.ConnectionLost(err) || len(sent.Rest) != 1 || sent.Rest[0].SID != "s5" {
		t.Fatal(sent, err)
	}
}

// ratings.out is taken by renaming it; what was not sent waits in ratings.out.sending and goes first.
func TestTakeOut(t *testing.T) {
	card := t.TempDir()
	dir := filepath.Join(card, ".subsonic")
	os.MkdirAll(dir, 0o755)
	out := filepath.Join(dir, OutName)
	os.WriteFile(out, []byte("k1\ts1\t3\t100\nbad line\nk2\t\t5\t101\n"), 0o644)
	got, err := TakeOut(card, dir)
	if err != nil || len(got) != 2 || got[0] != (Out{"k1", "s1", 3, 100}) || got[1].Key != "k2" {
		t.Fatal(got, err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ratings.out not taken")
	}
	os.WriteFile(out, []byte("k3\ts3\t1\t102\n"), 0o644) // the firmware's next change
	got, _ = TakeOut(card, dir)
	if len(got) != 3 || got[0].SID != "s1" || got[2].SID != "s3" {
		t.Fatal("not merged in order", got)
	}
	if err := KeepOut(card, dir, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := TakeOut(card, dir); len(got) != 0 {
		t.Fatal("kept", got)
	}
}

// ratings.in keeps what the firmware hasn't applied yet; a song's newer value replaces its older.
func TestWriteIn(t *testing.T) {
	card := t.TempDir()
	dir := filepath.Join(card, ".subsonic")
	os.MkdirAll(dir, 0o755)
	key := func(c Change) string { return "k" + c.SID }
	WriteIn(card, dir, []Change{{SID: "s1", Hearts: 3, At: 10}, {SID: "s2", Hearts: 5, At: 11}}, key)
	WriteIn(card, dir, []Change{{SID: "s1", Hearts: 0, At: 12}}, key)
	l := readLines(t, filepath.Join(dir, InName))
	if !slices.Equal(l, []string{"ks1\ts1\t0\t12", "ks2\ts2\t5\t11"}) {
		t.Fatal(l)
	}
	KeepOut(card, dir, []Out{{"ks3", "", 2, 13}}) // the Q2's own change of s3, not sent yet
	WriteIn(card, dir, []Change{{SID: "s3", Hearts: 4, At: 14}}, key)
	if l := readLines(t, filepath.Join(dir, InName)); len(l) != 2 {
		t.Fatal("a pending change of the Q2's overwritten", l)
	}
}

func TestAvail(t *testing.T) {
	card := t.TempDir()
	dir := filepath.Join(card, ".subsonic")
	os.MkdirAll(dir, 0o755)
	Avail(card, dir, "s1", "/mnt/mmc/.subsonic/stream/a [s1].flac")
	Avail(card, dir, "s1", "")
	if l := readLines(t, filepath.Join(dir, AvailName)); !slices.Equal(l, []string{"s1\t/mnt/mmc/.subsonic/stream/a [s1].flac", "-s1"}) {
		t.Fatal(l)
	}
	if Avail(card, filepath.Join(card, "Music"), "s1", "") == nil {
		t.Fatal("wrote outside the app's folder")
	}
}

// Another account's store is no store.
func TestStoreAccount(t *testing.T) {
	r := newRig(t, fakesonic.Library(2))
	r.walk(nil)
	if _, ok := r.st.Head("ffffffff"); ok {
		t.Fatal("head")
	}
	n := 0
	r.st.Each("ffffffff", func(Song) error { n++; return nil })
	if n != 0 {
		t.Fatal(n)
	}
}
