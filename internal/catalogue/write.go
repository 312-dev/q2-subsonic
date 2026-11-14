package catalogue

import (
	"path/filepath"
	"strconv"

	"github.com/andsuor/q2-subsonic/internal/library"
)

const catMagic = "#Q2CAT/1"

// Build is what a catalogue is written from besides the store and the playlists.
type Build struct {
	Card, App string // the card's root and the app's id: the folder is <card>/.<app>
	Account   string // 8 hex digits naming the signed-in account
	// Synced is the songs the app synced to the card, by id: their card paths. They are card rows
	// already, so they get no s row.
	Synced map[string]string
	// Local is the songs with a whole copy in the stream cache, by id: the copy's path.
	Local map[string]string
}

// Seq is the seq in path's header, 0 when there is none.
func Seq(path string) int64 {
	var seq int64
	lines(path, func(l string) error {
		kv, _ := readHeader(l, catMagic)
		seq, _ = strconv.ParseInt(kv["seq"], 10, 64)
		return errStop
	}, func(string) error { return nil })
	return seq
}

// Write writes catalogue.tsv at path, aside and renamed into place, with the next seq:
//
//	#Q2CAT/1	seq=<n>	account=<8 hex>	rows=<N>
//	s	<sid>	<title>	<artist>	<album artist>	<album>	<album id>	<year>	<disc>	<track>	<seconds>	<genre>	<suffix>	<local path or empty>	<size>	<musicBrainzId or empty>
//	p	<pid>	<playlist name>
//	i	<pid>	<sid>	<card path or empty>
//
// rows counts every s, p and i line. A song synced to the card has no s row, and a song gone from
// the server has one only while the stream cache keeps a copy of it (then it plays from that
// copy). A playlist's i lines name its songs in order: a synced song with its card path, a song
// with no row and no card copy not at all. The store is read twice, to count and to write, and
// never held in memory. Once the new catalogue is in place, catalogue.avail is emptied, as the
// catalogue's local paths are newer than anything in it. It returns the seq and rows written.
func Write(path string, st Store, pls []Playlist, b Build) (seq int64, rows int, err error) {
	keep := func(s Song) bool {
		if _, synced := b.Synced[s.ID]; synced {
			return false
		}
		return !s.Gone || b.Local[s.ID] != ""
	}
	want := map[string]bool{}
	for _, p := range pls {
		for _, sid := range p.SIDs {
			want[sid] = true
		}
	}
	listed := map[string]bool{} // the playlists' songs that have an s row
	err = st.Each(b.Account, func(s Song) error {
		if keep(s) {
			rows++
			if want[s.ID] {
				listed[s.ID] = true
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	for _, p := range pls {
		rows++
		for _, sid := range p.SIDs {
			if listed[sid] || b.Synced[sid] != "" {
				rows++
			}
		}
	}
	seq = Seq(path) + 1
	f, err := create(b.Card, path)
	if err != nil {
		return 0, 0, err
	}
	f.WriteString(header(catMagic, [][2]string{{"seq", strconv.FormatInt(seq, 10)}, {"account", b.Account},
		{"rows", itoa(rows)}}))
	err = st.Each(b.Account, func(s Song) error {
		if !keep(s) {
			return nil
		}
		_, err := f.WriteString(fields("s", s.ID, s.Title, s.Artist, s.AlbumArtist, s.Album, s.AlbumID, itoa(s.Year),
			itoa(s.Disc), itoa(s.Track), itoa(s.Seconds), s.Genre, s.Suffix, b.Local[s.ID],
			strconv.FormatInt(s.Size, 10), s.MBID))
		return err
	})
	if err != nil {
		f.abort()
		return 0, 0, err
	}
	for _, p := range pls {
		f.WriteString(fields("p", p.ID, p.Name))
		for _, sid := range p.SIDs {
			if listed[sid] || b.Synced[sid] != "" {
				f.WriteString(fields("i", p.ID, sid, b.Synced[sid]))
			}
		}
	}
	if err := f.commit(); err != nil {
		return 0, 0, err
	}
	library.Remove(b.Card, filepath.Join(filepath.Dir(path), AvailName))
	return seq, rows, nil
}

// Remove deletes the files the firmware reads (the catalogue and its availability), as when Show
// in Library is turned off or the account signs out. dir is the app's folder.
func Remove(card, dir string) {
	for _, n := range []string{CatName, AvailName, InName} {
		library.Remove(card, filepath.Join(dir, n))
		library.Remove(card, filepath.Join(dir, n+".tmp"))
	}
}

// The files' names in the app's folder.
const (
	CatName       = "catalogue.tsv"
	AvailName     = "catalogue.avail"
	OutName       = "ratings.out"
	InName        = "ratings.in"
	SongsName     = "server-songs.tsv"
	PlaylistsName = "server-playlists.tsv"
)
