package catalogue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/library"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// PlsOut is one line of playlists.out, a server playlist the Q2 changed (q2-coda's
// docs/server-library.md, Playlists from the Q2), in the card-app channel's escapes:
//
//	set\t<playlist id>\t<song id>...  its songs are these, in this order (after an add or a removal)
//	name\t<playlist id>\t<name>       renamed
//	delete\t<playlist id>             deleted
type PlsOut struct {
	Op, ID string
	Args   []string
}

func (o PlsOut) line() string { return fields(append([]string{o.Op, o.ID}, o.Args...)...) }

func readPls(path string) ([]PlsOut, error) {
	var out []PlsOut
	err := lines(path, nil, func(l string) error {
		f := split(l)
		if len(f) < 2 || f[1] == "" {
			return nil
		}
		switch {
		case f[0] == "set":
			sids := f[2:]
			if len(sids) == 1 && sids[0] == "" { // an empty playlist: "set\t<id>\t"
				sids = nil
			}
			out = append(out, PlsOut{"set", f[1], sids})
		case f[0] == "name" && len(f) == 3 && strings.TrimSpace(f[2]) != "":
			out = append(out, PlsOut{"name", f[1], f[2:]})
		case f[0] == "delete" && len(f) == 2:
			out = append(out, PlsOut{"delete", f[1], nil})
		}
		return nil
	})
	return out, err
}

// TakePls is the playlist changes still to send: an earlier connection's playlists.out.sending,
// then playlists.out's, taken by renaming it as TakeOut takes ratings.out.
func TakePls(card, dir string) ([]PlsOut, error) {
	sending, out := filepath.Join(dir, PlsOutName+".sending"), filepath.Join(dir, PlsOutName)
	taking := out + ".taking"
	rest, err := readPls(sending)
	if err != nil {
		return nil, err
	}
	if err := os.Rename(out, taking); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	more, err := readPls(taking)
	if err != nil {
		return nil, err
	}
	if len(more) > 0 {
		if err := KeepPls(card, dir, append(rest, more...)); err != nil {
			return nil, err
		}
	}
	library.Remove(card, taking)
	return append(rest, more...), nil
}

// KeepPls writes the lines still to send as playlists.out.sending, or removes it when there are none.
func KeepPls(card, dir string, rest []PlsOut) error {
	p := filepath.Join(dir, PlsOutName+".sending")
	if len(rest) == 0 {
		return library.Remove(card, p)
	}
	var b strings.Builder
	for _, o := range rest {
		b.WriteString(o.line())
	}
	return library.WriteOwned(card, p, []byte(b.String()))
}

// PlsPending is whether the firmware has written changes the app hasn't sent.
func PlsPending(dir string) bool {
	for _, n := range []string{PlsOutName, PlsOutName + ".sending"} {
		if fi, err := os.Stat(filepath.Join(dir, n)); err == nil && fi.Size() > 0 {
			return true
		}
	}
	return false
}

// PlsServer is what SendPls asks of the server (subsonic.Client).
type PlsServer interface {
	SetPlaylist(ctx context.Context, id string, ids []string) error
	RenamePlaylist(ctx context.Context, id, name string) error
	DeletePlaylist(ctx context.Context, id string) error
}

// SendPls sends the changes in order, each playlist's last "set" alone (an earlier one says less),
// and nothing for a playlist the lines go on to delete but the delete. A playlist the server no
// longer has, or refuses to change (another user's), is dropped; a lost server keeps the lines from
// the one it failed on for the next connection.
func SendPls(ctx context.Context, s PlsServer, outs []PlsOut) (sent int, rest []PlsOut, err error) {
	lastSet, deleted := map[string]int{}, map[string]bool{}
	for i, o := range outs {
		switch o.Op {
		case "set":
			lastSet[o.ID] = i
		case "delete":
			deleted[o.ID] = true
		}
	}
	for i, o := range outs {
		if deleted[o.ID] && o.Op != "delete" || o.Op == "set" && lastSet[o.ID] != i {
			continue
		}
		switch o.Op {
		case "set":
			err = s.SetPlaylist(ctx, o.ID, o.Args)
		case "name":
			err = s.RenamePlaylist(ctx, o.ID, o.Args[0])
		case "delete":
			err = s.DeletePlaylist(ctx, o.ID)
		}
		switch {
		case err == nil:
			sent++
		case errors.Is(err, subsonic.ErrNotFound):
			err = nil
		case subsonic.ConnectionLost(err) || ctx.Err() != nil:
			return sent, outs[i:], err // from this one on, sorted out again next time
		default: // refused (a playlist not the user's): not sent again
			err = nil
		}
	}
	return sent, nil, nil
}
