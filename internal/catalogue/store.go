package catalogue

import (
	"errors"
	"strconv"
)

const songsMagic = "#Q2SONGS/1"

// Hint is what the server last said about its library: getScanStatus's lastScan and count, or on
// a server without them getIndexes' lastModified. Either only says something may have changed.
type Hint struct {
	LastScan string
	Count    int64
	Modified int64 // milliseconds
}

// Store is server-songs.tsv: every song of the last walk, gone ones included, one a line after a
// header naming the account and the hint the walk was made under.
type Store struct {
	Card, Path string
}

// StoreHead is the store's header.
type StoreHead struct {
	Account string
	Hint    Hint
}

func (h StoreHead) line() string {
	return header(songsMagic, [][2]string{{"account", h.Account}, {"lastScan", h.Hint.LastScan},
		{"count", strconv.FormatInt(h.Hint.Count, 10)}, {"modified", strconv.FormatInt(h.Hint.Modified, 10)}})
}

func parseHead(l string) (StoreHead, bool) {
	kv, ok := readHeader(l, songsMagic)
	if !ok {
		return StoreHead{}, false
	}
	var h StoreHead
	h.Account, h.Hint.LastScan = kv["account"], kv["lastScan"]
	h.Hint.Count, _ = strconv.ParseInt(kv["count"], 10, 64)
	h.Hint.Modified, _ = strconv.ParseInt(kv["modified"], 10, 64)
	return h, true
}

func (s Song) line() string {
	gone := "0"
	if s.Gone {
		gone = "1"
	}
	return fields(s.ID, s.Title, s.Artist, s.AlbumArtist, s.Album, s.AlbumID, itoa(s.Year), itoa(s.Disc), itoa(s.Track),
		itoa(s.Seconds), s.Genre, s.Suffix, strconv.FormatInt(s.Size, 10), s.MBID, itoa(s.Rating), s.Starred, gone)
}

func parseSong(l string) (Song, bool) {
	f := split(l)
	if len(f) != 17 || f[0] == "" {
		return Song{}, false
	}
	n := func(i int) int { v, _ := strconv.Atoi(f[i]); return v }
	size, _ := strconv.ParseInt(f[12], 10, 64)
	return Song{ID: f[0], Title: f[1], Artist: f[2], AlbumArtist: f[3], Album: f[4], AlbumID: f[5], Year: n(6),
		Disc: n(7), Track: n(8), Seconds: n(9), Genre: f[10], Suffix: f[11], Size: size, MBID: f[13], Rating: n(14),
		Starred: f[15], Gone: f[16] == "1"}, true
}

// Head is the store's header, when it is account's; ok false when there is no store for it.
func (st Store) Head(account string) (h StoreHead, ok bool) {
	err := lines(st.Path, func(l string) error {
		h, ok = parseHead(l)
		return errStop
	}, func(string) error { return nil })
	if err != nil && err != errStop {
		return StoreHead{}, false
	}
	return h, ok && h.Account == account
}

var errStop = errors.New("stop") // ends a read early; never returned

// Each calls fn with every song of account's store, in its order. No store, or another account's,
// has none.
func (st Store) Each(account string, fn func(Song) error) error {
	mine := false
	err := lines(st.Path, func(l string) error {
		h, ok := parseHead(l)
		if !ok || h.Account != account {
			return errStop
		}
		mine = true
		return nil
	}, func(l string) error {
		if s, ok := parseSong(l); ok {
			return fn(s)
		}
		return nil
	})
	if err == errStop || !mine {
		return nil
	}
	return err
}

// Rewrite writes the store again with each song as edit returns it (keep false drops it).
func (st Store) Rewrite(account string, edit func(Song) (Song, bool)) error {
	h, ok := st.Head(account)
	if !ok {
		return nil
	}
	f, err := create(st.Card, st.Path)
	if err != nil {
		return err
	}
	f.WriteString(h.line())
	if err := st.Each(account, func(s Song) error {
		if s, keep := edit(s); keep {
			_, err := f.WriteString(s.line())
			return err
		}
		return nil
	}); err != nil {
		f.abort()
		return err
	}
	return f.commit()
}

// Find is the songs of ids that the store has, by id.
func (st Store) Find(account string, ids map[string]bool) map[string]Song {
	out := map[string]Song{}
	st.Each(account, func(s Song) error {
		if ids[s.ID] {
			out[s.ID] = s
		}
		return nil
	})
	return out
}
