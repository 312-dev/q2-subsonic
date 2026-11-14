// Package catalogue is the app's half of Q2 Pod's Server Library (q2-coda's docs/server-library.md):
// the server's songs and playlists that are not on the card, written for the firmware to show in
// its Library beside the card's own, and the hearts that follow a song between the Q2 and the
// server.
//
// Every file is in the app's folder on the card (.subsonic/), each written whole and renamed into
// place unless it says otherwise:
//
//	server-songs.tsv      the app's own record of every server song, synced ones included, with its
//	                      rating, star and whether the server still has it (Store)
//	server-playlists.tsv  the server's playlists, their changed dates and songs (Playlists)
//	catalogue.tsv         what the firmware imports: the songs not synced, and the playlists (Write)
//	catalogue.avail       a line appended for each song the stream cache gains or loses (Avail)
//	ratings.out           the firmware's: hearts changed on the Q2, taken by renaming (TakeOut)
//	ratings.in            the server's hearts that differ from what the Q2 was told (WriteIn)
//
// No file holds the whole library in memory: songs are read and written a line at a time.
package catalogue

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Song is one server song as the Server Library keeps it.
type Song struct {
	ID, Title, Artist, AlbumArtist, Album, AlbumID string
	Year, Disc, Track, Seconds                     int
	Genre, Suffix                                  string
	Size                                           int64
	MBID                                           string // MusicBrainz recording id, "" when the server has none
	Rating                                         int    // the user's, 0 to 5
	Starred                                        string // when the user starred it, as the server wrote it; "" when not
	Gone                                           bool   // a complete walk no longer found it on the server
}

// FromInfo is the song the server described.
func FromInfo(s subsonic.SongInfo) Song {
	return Song{ID: s.ID, Title: s.Title, Artist: s.Artist, AlbumArtist: s.AlbumArtist, Album: s.Album, AlbumID: s.AlbumID,
		Year: s.Year, Disc: s.Disc, Track: s.Track, Seconds: s.Duration, Genre: s.Genre, Suffix: s.Suffix, Size: s.Size,
		MBID: s.MBID, Rating: s.Rating, Starred: s.Starred}
}

// Subsonic is the song as the streamer and the player take it.
func (s Song) Subsonic() subsonic.Song {
	return subsonic.Song{ID: s.ID, Title: s.Title, Album: s.Album, AlbumID: s.AlbumID, Artist: s.Artist,
		AlbumArtist: s.AlbumArtist, Year: s.Year, Disc: s.Disc, Track: s.Track, Duration: s.Seconds, Size: s.Size,
		Suffix: s.Suffix}
}

// Hearts is the song's hearts on the Q2's scale: a star is five (stock's My Fav), else the rating.
func (s Song) Hearts() int {
	if s.Starred != "" {
		return 5
	}
	return min(max(s.Rating, 0), 5)
}

// FileURL is the path the firmware gives a server row (its identity, never a file that exists):
// <card>/.<app>/cat/<album id>/<sid>.<suffix>, built as the firmware builds it, without cleaning.
func FileURL(card, app, albumID, sid, suffix string) string {
	return card + "/." + app + "/cat/" + albumID + "/" + sid + "." + suffix
}

// SIDOf is the song id of a server row's FileURL, "" for any other path.
func SIDOf(card, app, path string) string {
	rest, ok := strings.CutPrefix(path, card+"/."+app+"/cat/")
	if !ok {
		return ""
	}
	i := strings.LastIndexByte(rest, '/')
	if i < 0 {
		return ""
	}
	name := rest[i+1:]
	if j := strings.LastIndexByte(name, '.'); j > 0 {
		name = name[:j]
	}
	return name
}

// fields joins values with tabs, each with the card-app channel's escapes (Q2SPOT/1's: %, tab,
// LF and CR as %25, %09, %0A and %0D).
func fields(vs ...string) string {
	for i, v := range vs {
		vs[i] = q2spot.Escape(v)
	}
	return strings.Join(vs, "\t") + "\n"
}

// split is fields' reverse; a field that isn't well escaped is taken as it is.
func split(line string) []string {
	f := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
	for i, v := range f {
		if u, err := q2spot.Unescape(v); err == nil {
			f[i] = u
		}
	}
	return f
}

func itoa(n int) string { return strconv.Itoa(n) }

// header is a file's first line: its magic and key=value fields.
func header(magic string, kv [][2]string) string {
	var b strings.Builder
	b.WriteString(magic)
	for _, p := range kv {
		b.WriteString("\t" + p[0] + "=" + q2spot.Escape(p[1]))
	}
	return b.String() + "\n"
}

func readHeader(line, magic string) (map[string]string, bool) {
	f := split(line)
	if len(f) == 0 || f[0] != magic {
		return nil, false
	}
	kv := map[string]string{}
	for _, p := range f[1:] {
		if k, v, ok := strings.Cut(p, "="); ok {
			kv[k] = v
		}
	}
	return kv, true
}

// file is a card file being written: <path>.tmp, renamed into place by commit. Only paths the app
// owns (library.Owned) are written.
type file struct {
	path string
	f    *os.File
	w    *bufio.Writer
}

func create(card, path string) (*file, error) {
	if filepath.Clean(path) != path || !library.Owned(card, path, nil) {
		return nil, errors.New("refusing path outside owned trees: " + path)
	}
	f, err := os.Create(path + ".tmp")
	if err != nil {
		return nil, err
	}
	return &file{path, f, bufio.NewWriterSize(f, 64<<10)}, nil
}

func (f *file) WriteString(s string) (int, error) { return f.w.WriteString(s) }

func (f *file) commit() error {
	err := f.w.Flush()
	if err == nil {
		err = f.f.Sync()
	}
	if cerr := f.f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.path+".tmp", f.path)
	}
	if err != nil {
		os.Remove(f.path + ".tmp")
	}
	return err
}

func (f *file) abort() {
	f.f.Close()
	os.Remove(f.path + ".tmp")
}

// lines calls fn with each line of path after its first (the header, given to head). A missing
// file has no lines and no error.
func lines(path string, head func(string) error, fn func(string) error) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for first := true; ; first = false {
		l, err := r.ReadString('\n')
		if l != "" {
			if first && head != nil {
				if herr := head(l); herr != nil {
					return herr
				}
			} else if !first || head == nil {
				if ferr := fn(l); ferr != nil {
					return ferr
				}
			}
		}
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
	}
}
