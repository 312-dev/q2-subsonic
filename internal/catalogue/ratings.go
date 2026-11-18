package catalogue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// ListenKey is the firmware's listen_key of a card path (q2-coda's patch/navigation.c: FNV-1a 32
// of the path's bytes, one more multiply by the prime, and never 0), as ratings.out and ratings.in
// write it: 8 lowercase hex digits. A CUE track's start is hashed in too; the app's files have none.
func ListenKey(path string) string {
	h := uint32(2166136261)
	for i := 0; i < len(path); i++ {
		h = (h ^ uint32(path[i])) * 16777619
	}
	h *= 16777619
	if h == 0 {
		h = 1
	}
	return fmt.Sprintf("%08x", h)
}

// Out is one line of ratings.out, a change made on the Q2:
// <listen_key>\t<sid or empty>\t<value>\t<unix time>, value stars=N (0 to 5) or like=0|1, as Q2 Muse
// writes them for an app with ratings=2 (q2-coda's docs/server-library.md, Ratings). A line of one
// number, the format before likes and stars were apart, is both: that many stars, and a like at
// five. A kind a line doesn't carry is -1.
type Out struct {
	Key, SID    string
	Stars, Like int // 0 to 5, and 0 or 1; -1 when the line doesn't say
	At          int64
}

// lines is o as ratings.out lines: its stars, its like, or both as two lines.
func (o Out) lines() string {
	var b strings.Builder
	at := strconv.FormatInt(o.At, 10)
	if o.Stars >= 0 {
		b.WriteString(fields(o.Key, o.SID, "stars="+itoa(o.Stars), at))
	}
	if o.Like >= 0 {
		b.WriteString(fields(o.Key, o.SID, "like="+itoa(o.Like), at))
	}
	return b.String()
}

// parseValue reads a value column: stars=N, like=0|1, or the older single number.
func parseValue(v string) (stars, like int, ok bool) {
	if n, found := strings.CutPrefix(v, "stars="); found {
		s, err := strconv.Atoi(n)
		return s, -1, err == nil && s >= 0 && s <= 5
	}
	if n, found := strings.CutPrefix(v, "like="); found {
		return -1, map[string]int{"0": 0, "1": 1}[n], n == "0" || n == "1"
	}
	h, err := strconv.Atoi(v)
	if err != nil || h < 0 || h > 5 {
		return 0, 0, false
	}
	like = 0
	if h == 5 {
		like = 1
	}
	return h, like, true
}

func readOut(path string) ([]Out, error) {
	var out []Out
	err := lines(path, nil, func(l string) error {
		f := split(l)
		if len(f) != 4 {
			return nil
		}
		stars, like, ok := parseValue(f[2])
		at, err := strconv.ParseInt(f[3], 10, 64)
		if !ok || err != nil || f[0] == "" && f[1] == "" {
			return nil
		}
		out = append(out, Out{f[0], f[1], stars, like, at})
		return nil
	})
	return out, err
}

// TakeOut is the changes made on the Q2 that are still to send: those an earlier connection left
// in ratings.out.sending, then ratings.out's. The firmware only appends to ratings.out, so it is
// taken by renaming it (the firmware's next change starts a new one), and its lines join
// ratings.out.sending, which the app alone writes. The lines are in the order they were made.
func TakeOut(card, dir string) ([]Out, error) {
	sending, out := filepath.Join(dir, OutName+".sending"), filepath.Join(dir, OutName)
	taking := out + ".taking"
	rest, err := readOut(sending)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(out, taking); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	more, err := readOut(taking) // one an earlier run renamed and then failed to join, too
	if err != nil {
		return nil, err
	}
	if len(more) > 0 {
		if err := KeepOut(card, dir, append(rest, more...)); err != nil {
			return nil, err
		}
	}
	library.Remove(card, taking)
	return append(rest, more...), nil
}

// KeepOut writes the lines still to send as ratings.out.sending, or removes it when there are none.
func KeepOut(card, dir string, rest []Out) error {
	p := filepath.Join(dir, OutName+".sending")
	if len(rest) == 0 {
		return library.Remove(card, p)
	}
	var b strings.Builder
	for _, o := range rest {
		b.WriteString(o.lines())
	}
	return library.WriteOwned(card, p, []byte(b.String()))
}

// Rater is what Send asks of the server (subsonic.Client).
type Rater interface {
	SongInfo(ctx context.Context, id string) (subsonic.SongInfo, error)
	SetRating(ctx context.Context, id string, rating int) error
	Star(ctx context.Context, id string) error
	Unstar(ctx context.Context, id string) error
}

// Sent is what Send did.
type Sent struct {
	Values  map[string]Rated // the songs whose stars and like the server now has from the Q2, by id
	Wins    []Change         // songs whose server star is newer than the Q2's like: the server's like wins
	Rest    []Out            // the lines not sent, to try at the next connection
	Dropped int              // lines naming no song the app knows, or one the server no longer has
}

// Send sends the Q2's changes, the last of each kind of each song, before anything is read back
// from the server (a rating carries no date there, so a change waiting on the Q2 is taken as the
// newer). Stars go with setRating, a like with star or unstar, each on its own: a line of stars
// leaves the server's star as it is, and a like its rating. Only a star has a date: a song starred
// on the server after the Q2's like changed keeps the server's star, which goes back to the Q2 as a
// Change. What the server already has is not sent. sidOf finds the song of a line without a sid (a
// synced card file, by its listen_key). A lost connection stops it, with the lines left in Rest;
// any other failure keeps that song's lines for the next time.
func Send(ctx context.Context, r Rater, outs []Out, sidOf func(key string) string) (Sent, error) {
	res := Sent{Values: map[string]Rated{}}
	want := map[string]*Out{} // sid -> its last stars and its last like, merged
	var order []string
	for _, o := range outs {
		sid := o.SID
		if sid == "" && sidOf != nil {
			sid = sidOf(o.Key)
		}
		if sid == "" {
			res.Dropped++
			continue
		}
		w := want[sid]
		if w == nil {
			w = &Out{Key: o.Key, SID: sid, Stars: -1, Like: -1}
			want[sid] = w
			order = append(order, sid)
		}
		if o.Stars >= 0 {
			w.Stars = o.Stars
		}
		if o.Like >= 0 {
			w.Like = o.Like
		}
		w.At = max(w.At, o.At)
	}
	for k, sid := range order {
		o := *want[sid]
		err := send(ctx, r, o, &res)
		switch {
		case err == nil:
		case errors.Is(err, subsonic.ErrNotFound):
			res.Dropped++
		case subsonic.ConnectionLost(err) || ctx.Err() != nil:
			for _, sid := range order[k:] {
				res.Rest = append(res.Rest, *want[sid])
			}
			return res, err
		default:
			res.Rest = append(res.Rest, o)
		}
	}
	return res, nil
}

func send(ctx context.Context, r Rater, o Out, res *Sent) error {
	si, err := r.SongInfo(ctx, o.SID)
	if err != nil {
		return err
	}
	s := FromInfo(si)
	v := s.Rated()
	if o.Stars >= 0 && o.Stars != v.Stars {
		if err := r.SetRating(ctx, o.SID, o.Stars); err != nil {
			return err
		}
	}
	if o.Stars >= 0 {
		v.Stars = o.Stars
	}
	if like := o.Like == 1; o.Like >= 0 && like != v.Like {
		if t, err := time.Parse(time.RFC3339, s.Starred); err == nil && t.Unix() > o.At {
			res.Wins = append(res.Wins, Change{SID: o.SID, Song: s, Stars: v.Stars, Like: v.Like, At: t.Unix()})
		} else {
			if like {
				err = r.Star(ctx, o.SID)
			} else {
				err = r.Unstar(ctx, o.SID)
			}
			if err != nil {
				return err
			}
			v.Like = like
		}
	}
	res.Values[o.SID] = v
	return nil
}

// WriteIn adds changes to ratings.in, two lines a song in ratings.out's columns, its stars
// (<listen_key>\t<sid>\tstars=N\t<unix time>) and its like (like=0|1), written whole. The firmware
// applies the file and deletes it; one still there when more come keeps its lines, a song's newer
// ones replacing its older. A song with a change of the Q2's still to send (ratings.out.sending) is
// left out: the Q2's is the newer.
func WriteIn(card, dir string, changes []Change, key func(Change) string) error {
	pending, err := readOut(filepath.Join(dir, OutName+".sending"))
	if err != nil {
		return err
	}
	wait := map[string]bool{}
	for _, o := range pending {
		wait[o.SID], wait[o.Key] = true, true
	}
	var take []Change
	for _, c := range changes {
		if !wait[c.SID] && !wait[key(c)] {
			take = append(take, c)
		}
	}
	if changes = take; len(changes) == 0 {
		return nil
	}
	p := filepath.Join(dir, InName)
	old, err := readOut(p)
	if err != nil {
		return err
	}
	now := map[string]Out{} // each changed song's values, in place of its lines in the file
	for _, c := range changes {
		like := 0
		if c.Like {
			like = 1
		}
		now[c.SID] = Out{key(c), c.SID, c.Stars, like, c.At}
	}
	var b strings.Builder
	for _, o := range old {
		if n, ok := now[o.SID]; ok {
			o = n
			if n.Key == "" { // written already, at the song's first line
				continue
			}
			now[o.SID] = Out{}
		}
		b.WriteString(o.lines())
	}
	for _, c := range changes {
		if o := now[c.SID]; o.Key != "" {
			b.WriteString(o.lines())
			now[c.SID] = Out{}
		}
	}
	return library.WriteOwned(card, p, []byte(b.String()))
}
