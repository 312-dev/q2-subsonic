package catalogue

import (
	"context"
	"slices"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

const playlistsMagic = "#Q2PLAYLISTS/1"

// Playlist is a server playlist: its songs' ids in order, duplicates included, and when the
// server says it last changed.
type Playlist struct {
	ID, Name, Changed string
	SIDs              []string
}

// Playlists is server-playlists.tsv: a "p" line a playlist (id, name, changed), each followed by an
// "i" line a song (the song's id).
type Playlists struct {
	Card, Path string
}

// Load is account's playlists as last saved; ok false when none were.
func (ps Playlists) Load(account string) (out []Playlist, ok bool) {
	err := lines(ps.Path, func(l string) error {
		kv, good := readHeader(l, playlistsMagic)
		if !good || kv["account"] != account {
			return errStop
		}
		ok = true
		return nil
	}, func(l string) error {
		f := split(l)
		switch {
		case len(f) == 4 && f[0] == "p":
			out = append(out, Playlist{ID: f[1], Name: f[2], Changed: f[3]})
		case len(f) == 2 && f[0] == "i" && len(out) > 0:
			out[len(out)-1].SIDs = append(out[len(out)-1].SIDs, f[1])
		}
		return nil
	})
	if err != nil {
		return nil, false
	}
	return out, ok
}

func (ps Playlists) Save(account string, pls []Playlist) error {
	f, err := create(ps.Card, ps.Path)
	if err != nil {
		return err
	}
	f.WriteString(header(playlistsMagic, [][2]string{{"account", account}}))
	for _, p := range pls {
		f.WriteString(fields("p", p.ID, p.Name, p.Changed))
		for _, sid := range p.SIDs {
			f.WriteString(fields("i", sid))
		}
	}
	return f.commit()
}

// Lister is the server's playlists (subsonic.Client).
type Lister interface {
	Playlists(ctx context.Context) ([]subsonic.Playlist, error)
	Playlist(ctx context.Context, id string) (subsonic.Playlist, []subsonic.Song, error)
}

// Refresh is the server's playlists now: each one whose changed date (and name) is the one in old
// keeps its songs, and only the others are asked for, so a connection costs one request when
// nothing changed. A playlist without a date is asked for every time. changed reports whether
// what the catalogue shows differs from old: a playlist added, removed, moved, renamed or with
// other songs. A date alone is no change (Navidrome moves a smart playlist's when it is asked for
// and finds the same songs), but the dates are worth saving (SameDates).
func Refresh(ctx context.Context, l Lister, old []Playlist) (pls []Playlist, changed bool, err error) {
	list, err := l.Playlists(ctx)
	if err != nil {
		return nil, false, err
	}
	byID := map[string]Playlist{}
	for _, p := range old {
		byID[p.ID] = p
	}
	changed = len(list) != len(old)
	for i, sp := range list {
		o, had := byID[sp.ID]
		if had && sp.Changed != "" && o.Changed == sp.Changed && o.Name == sp.Name {
			pls = append(pls, o)
			changed = changed || i >= len(old) || old[i].ID != sp.ID // moved in the list
			continue
		}
		_, songs, err := l.Playlist(ctx, sp.ID)
		if err != nil {
			return nil, false, err
		}
		p := Playlist{ID: sp.ID, Name: sp.Name, Changed: sp.Changed}
		for _, s := range songs {
			p.SIDs = append(p.SIDs, s.ID)
		}
		changed = changed || !had || o.Name != p.Name || !slices.Equal(o.SIDs, p.SIDs) || i >= len(old) || old[i].ID != sp.ID
		pls = append(pls, p)
	}
	return pls, changed, nil
}

// SameDates reports whether a and b are the same playlists with the same changed dates.
func SameDates(a, b []Playlist) bool {
	return slices.EqualFunc(a, b, func(x, y Playlist) bool { return x.ID == y.ID && x.Changed == y.Changed })
}
