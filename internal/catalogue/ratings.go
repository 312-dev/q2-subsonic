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

// Out is one line of ratings.out, a change of hearts made on the Q2:
// <listen_key>\t<sid or empty>\t<hearts>\t<unix time>.
type Out struct {
	Key, SID string
	Hearts   int
	At       int64
}

func (o Out) line() string {
	return fields(o.Key, o.SID, itoa(o.Hearts), strconv.FormatInt(o.At, 10))
}

func readOut(path string) ([]Out, error) {
	var out []Out
	err := lines(path, nil, func(l string) error {
		f := split(l)
		if len(f) != 4 {
			return nil
		}
		h, err1 := strconv.Atoi(f[2])
		at, err2 := strconv.ParseInt(f[3], 10, 64)
		if err1 != nil || err2 != nil || h < 0 || h > 5 || f[0] == "" && f[1] == "" {
			return nil
		}
		out = append(out, Out{f[0], f[1], h, at})
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
		b.WriteString(o.line())
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
	Hearts  map[string]int // the songs whose hearts the server now has from the Q2, by id
	Wins    []Change       // songs whose server star is newer than the Q2's change: the server's hearts win
	Rest    []Out          // the lines not sent, to try at the next connection
	Dropped int            // lines naming no song the app knows, or one the server no longer has
}

// Send sends the Q2's changes, the last one of each song, before anything is read back from the
// server (a rating carries no date there, so a change waiting on the Q2 is taken as the newer).
// A song is setRating to its hearts, 0 to 5; five hearts also stars it, fewer unstars it when it
// was starred. Only a star has a date: a song starred on the server after the Q2's change keeps
// the server's hearts, which go back to the Q2 as a Change. A song whose server hearts already
// match is sent nothing. sidOf finds the song of a line without a sid (a synced card file, by its
// listen_key). A lost connection stops it, with the lines left in Rest; any other failure keeps
// that line for the next time.
func Send(ctx context.Context, r Rater, outs []Out, sidOf func(key string) string) (Sent, error) {
	res := Sent{Hearts: map[string]int{}}
	last := map[string]int{} // sid -> index of its last line
	var order []string
	for i, o := range outs {
		sid := o.SID
		if sid == "" && sidOf != nil {
			sid = sidOf(o.Key)
		}
		if sid == "" {
			res.Dropped++
			continue
		}
		outs[i].SID = sid
		if _, ok := last[sid]; !ok {
			order = append(order, sid)
		}
		last[sid] = i
	}
	for k, sid := range order {
		o := outs[last[sid]]
		err := send(ctx, r, o, &res)
		switch {
		case err == nil:
		case errors.Is(err, subsonic.ErrNotFound):
			res.Dropped++
		case subsonic.ConnectionLost(err) || ctx.Err() != nil:
			for _, sid := range order[k:] {
				res.Rest = append(res.Rest, outs[last[sid]])
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
	if s.Hearts() == o.Hearts {
		res.Hearts[o.SID] = o.Hearts
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s.Starred); err == nil && t.Unix() > o.At {
		res.Wins = append(res.Wins, Change{SID: o.SID, Song: s, Hearts: s.Hearts(), At: t.Unix()})
		return nil
	}
	if err := r.SetRating(ctx, o.SID, o.Hearts); err != nil {
		return err
	}
	switch {
	case o.Hearts == 5 && s.Starred == "":
		err = r.Star(ctx, o.SID)
	case o.Hearts < 5 && s.Starred != "":
		err = r.Unstar(ctx, o.SID)
	}
	if err != nil {
		return err
	}
	res.Hearts[o.SID] = o.Hearts
	return nil
}

// WriteIn adds changes to ratings.in, a line a song (<listen_key>\t<sid>\t<hearts>\t<unix time>,
// the columns of ratings.out), written whole. The firmware applies the file and deletes it; one
// still there when more come keeps its lines, a song's newer one replacing its older.
func WriteIn(card, dir string, changes []Change, key func(Change) string) error {
	if len(changes) == 0 {
		return nil
	}
	p := filepath.Join(dir, InName)
	old, err := readOut(p)
	if err != nil {
		return err
	}
	idx := map[string]int{}
	for i, o := range old {
		idx[o.SID] = i
	}
	for _, c := range changes {
		o := Out{key(c), c.SID, c.Hearts, c.At}
		if i, ok := idx[c.SID]; ok {
			old[i] = o
			continue
		}
		idx[c.SID] = len(old)
		old = append(old, o)
	}
	var b strings.Builder
	for _, o := range old {
		b.WriteString(o.line())
	}
	return library.WriteOwned(card, p, []byte(b.String()))
}
