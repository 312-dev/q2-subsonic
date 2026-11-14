package catalogue

import (
	"context"
	"errors"
	"time"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Page is the songs asked for at a time.
const Page = 500

// Pager is the server's songs a page at a time (subsonic.Client.SongInfos: search3 with the
// empty query, which OpenSubsonic requires servers to take for offline sync).
type Pager interface {
	SongInfos(ctx context.Context, offset, size int) ([]subsonic.SongInfo, error)
}

// Change is a song's hearts as the server has them, for the Q2.
type Change struct {
	SID    string
	Song   Song // the song as the walk or the star check found it
	Hearts int
	At     int64 // when the server's value was set: the star's time, else when it was read
}

// WalkResult is what a walk found.
type WalkResult struct {
	Songs, Gone, Requests int
	Complete              bool     // every song the server counts was seen: the songs not seen are gone
	Changes               []Change // hearts that differ from the store's
}

// ErrNoSongs is a walk that found nothing on a server where the store had songs: a server that
// doesn't take the empty query, never a library emptied, so nothing is marked gone.
var ErrNoSongs = errors.New("the server listed no songs")

// now is the clock a Change's time falls back on; tests set it.
var now = time.Now

// Walk asks for every song, Page at a time, and writes each page to the store's file as it
// arrives (aside, renamed once the walk ends; a failed walk leaves the store as it was). Paging
// stops at a short page, or at one with no song not seen before (a server that ignores the
// offset). A song in the store that the walk didn't see is kept and marked gone, never removed:
// Navidrome hides a missing file and gives it its id again when it comes back. That is only when
// the walk is complete (it saw hint.Count songs, or the hint has no count); else the songs not
// seen are kept as they were. sent is the hearts just sent to the server, which its answers may
// not show yet. note is told how many songs have come.
func (st Store) Walk(ctx context.Context, p Pager, account string, hint Hint, sent map[string]int, note func(int)) (WalkResult, error) {
	var res WalkResult
	prev := map[string]int8{}
	if err := st.Each(account, func(s Song) error {
		prev[s.ID] = int8(s.Hearts())
		return nil
	}); err != nil {
		return res, err
	}
	for id, h := range sent {
		prev[id] = int8(h)
	}
	f, err := create(st.Card, st.Path)
	if err != nil {
		return res, err
	}
	f.WriteString(StoreHead{account, hint}.line())
	seen := map[string]bool{}
	for off := 0; ; {
		page, err := p.SongInfos(ctx, off, Page)
		res.Requests++
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			f.abort()
			return res, err
		}
		n := len(seen)
		for _, si := range page {
			if si.ID == "" || seen[si.ID] {
				continue
			}
			seen[si.ID] = true
			s := FromInfo(si)
			if _, err := f.WriteString(s.line()); err != nil {
				f.abort()
				return res, err
			}
			old, had := prev[s.ID]
			if h := s.Hearts(); int(old) != h && (had || h > 0) {
				res.Changes = append(res.Changes, Change{SID: s.ID, Song: s, Hearts: h, At: changedAt(s)})
			}
		}
		if note != nil {
			note(len(seen))
		}
		if len(page) < Page || len(seen) == n {
			break
		}
		off += len(page)
	}
	if len(seen) == 0 && len(prev) > 0 {
		f.abort()
		return res, ErrNoSongs
	}
	res.Songs = len(seen)
	res.Complete = hint.Count <= 0 || int64(len(seen)) >= hint.Count
	err = lines(st.Path, func(l string) error {
		if h, ok := parseHead(l); !ok || h.Account != account {
			return errStop
		}
		return nil
	}, func(l string) error {
		s, ok := parseSong(l)
		if !ok || seen[s.ID] {
			return nil
		}
		s.Gone = s.Gone || res.Complete
		if s.Gone {
			res.Gone++
		}
		_, err := f.WriteString(s.line())
		return err
	})
	if err != nil && err != errStop {
		f.abort()
		return res, err
	}
	return res, f.commit()
}

// changedAt is when the server's hearts for s were set: its star's time, else now.
func changedAt(s Song) int64 {
	if t, err := time.Parse(time.RFC3339, s.Starred); err == nil {
		return t.Unix()
	}
	return now().Unix()
}

// StarChanges compares the songs the server has starred (getStarred2) with the store: a song
// starred that the store has unstarred, or the other way, is a Change, and the store is written
// again with the new stars. Run on every connection: a star changes without a scan. sent is as
// for Walk.
func (st Store) StarChanges(account string, starred []subsonic.SongInfo, sent map[string]int) ([]Change, error) {
	now := map[string]string{}
	for _, s := range starred {
		now[s.ID] = s.Starred
	}
	var out []Change
	edit := func(s Song) (Song, bool) {
		at, on := now[s.ID]
		if s.Gone || on == (s.Starred != "") {
			return s, true
		}
		before := s.Hearts()
		if h, ok := sent[s.ID]; ok {
			before = h
		}
		if on {
			s.Starred = at
			if at == "" {
				s.Starred = "1" // a server that names no time: starred all the same
			}
		} else {
			s.Starred = ""
		}
		if s.Hearts() != before {
			out = append(out, Change{SID: s.ID, Song: s, Hearts: s.Hearts(), At: changedAt(s)})
		}
		return s, true
	}
	changed := false
	if err := st.Each(account, func(s Song) error {
		if _, on := now[s.ID]; !s.Gone && on != (s.Starred != "") {
			changed = true
			return errStop
		}
		return nil
	}); err != nil && err != errStop {
		return nil, err
	}
	if !changed {
		return nil, nil
	}
	return out, st.Rewrite(account, edit)
}
