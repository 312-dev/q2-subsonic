package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// Shuffle and radio, and an artist's Play, Shuffle and Top Songs. All of it is plain rows and the
// streamer: no firmware word is needed.

const (
	shuffleSongs = 100 // songs a Shuffle asks the server for
	radioSongs   = 50  // songs of a Radio, and of Top Songs
	// artistPlayIn is how long an artist's Play or Shuffle may fetch its albums' songs.
	artistPlayIn = time.Minute
	// maxGenre is the longest index group's name listed (Artists' letters), escaped: a row's context must fit the firmware's
	// 400 bytes of level arguments with its cursor, or a press on it does nothing.
	maxGenre = 300
	notHere  = "Not available on this server" // Top Songs or Radio on a server without it
)

func (v view) album(a subsonic.Album) q2spot.Row {
	return v.item(selection.KindAlbum, a.ID, a.Name, a.Artist, a.SongCount)
}

// albumList is one page of an album list the server pages (typ is getAlbumList2's type, genre
// byGenre's genre): head, then the albums. The cursor counts rows, head included, as withLead
// and fit set it; one album more than a page is asked for, which says whether More… follows. With
// "window" the reply is a window's (windowed).
func (d *daemon) albumList(ctx context.Context, cl *subsonic.Client, typ, genre string, head []q2spot.Row, cursor string) q2spot.Reply {
	cur, _ := strconv.Atoi(cursor)
	rows := q2spot.MaxRows
	if d.caps.Window {
		rows = winRoom(max(cur, 0))
	}
	off, room := 0, rows-len(head)
	if cur > 0 {
		off, room, head = max(cur-len(head), 0), rows, nil
	}
	al, err := cl.AlbumList(ctx, typ, genre, off, room+1)
	if err != nil {
		return d.listErr(err, cursor)
	}
	more := len(al) > room
	if more {
		al = al[:room]
	}
	v := d.view()
	r := q2spot.Reply{Rows: head}
	covers := map[string]string{}
	for _, a := range al {
		row := coverRow(v.album(a), covers, a.CoverArt)
		if typ == "byYear" && a.Year > 0 {
			row.Subtitle = join(strconv.Itoa(a.Year), row.Subtitle)
		}
		r.Rows = append(r.Rows, row)
	}
	if d.caps.Window {
		r = windowed(r.Rows, cur, max(cur, 0), more)
	} else if n := fit(r.Rows); n < len(r.Rows) {
		r.Rows, more = r.Rows[:n], true
	}
	if more && !d.caps.Window {
		r.Cursor = strconv.Itoa(max(cur, 0) + len(r.Rows))
	}
	if len(al) == 0 && cur <= 0 {
		r.Rows = append(r.Rows, info("No albums"))
	}
	return d.withArt(r, covers, cursor)
}

// starredFound is what is starred, held for browseTTL only: a server may have thousands.
func (d *daemon) starredFound(ctx context.Context, cl *subsonic.Client) (found, error) {
	return cached(d, cl, "starred", func() (f found, err error) {
		f.ars, f.als, f.songs, err = cl.Starred(ctx)
		return f, err
	})
}

func (d *daemon) top(ctx context.Context, cl *subsonic.Client, arg, cursor string) q2spot.Reply {
	_, id := parseKey(arg)
	key := "top:" + id
	songs, err := d.topSongs(ctx, cl, cl, id)
	switch {
	case subsonic.Unsupported(err):
		return q2spot.Reply{Rows: []q2spot.Row{info(notHere)}}
	case errors.Is(err, subsonic.ErrNotFound):
		songs = nil
	case err != nil:
		return d.listErr(err, cursor)
	}
	d.mu.Lock()
	d.listed = songList{key, songs}
	d.mu.Unlock()
	v := d.view()
	var rows []q2spot.Row
	for i, s := range songs {
		rows = append(rows, v.listSong(s, key, i, d.caps.Artist))
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("No top songs")}
	}
	return d.page(rows, cursor)
}

// topSongs is an artist's Top Songs, kept under Top Songs' own key, so the artist's page that asked
// first leaves them ready for it. call makes the request (cl, or cl within a time limit); cl is the
// account's client, which the cache is kept by.
func (d *daemon) topSongs(ctx context.Context, cl, call *subsonic.Client, id string) ([]subsonic.Song, error) {
	return cached(d, cl, "top:"+id, func() ([]subsonic.Song, error) {
		it, err := d.item(ctx, cl, selection.KindArtist, id)
		if err != nil {
			return nil, err
		}
		return call.TopSongs(ctx, it.name, radioSongs)
	})
}

// playNow streams songs from the first, or says there is nothing.
func (d *daemon) playNow(songs []subsonic.Song, none string) q2spot.Reply {
	if len(songs) == 0 {
		return q2spot.Fail(q2spot.Unavailable, none)
	}
	d.startStream(d.nextPlay(), songs, 0)
	return q2spot.Reply{Message: "Playing " + songs[0].Title}
}

// shuffle plays random songs: of the whole server (""), of a genre ("genre:<name>"), or an
// artist's or album's own in random order ("artist:<id>", "album:<id>"). The argument is cut at
// its first colon only: a genre's name may hold spaces and colons.
func (d *daemon) shuffle(ctx context.Context, cl *subsonic.Client, arg string) q2spot.Reply {
	what, name, _ := strings.Cut(arg, ":")
	if what == string(selection.KindArtist) {
		return d.playArtist(ctx, cl, name, true)
	}
	if what == string(selection.KindAlbum) {
		it, err := d.item(ctx, cl, selection.KindAlbum, name)
		if err != nil {
			return d.errToast(err)
		}
		return d.playNow(shuffled(it.songs), "Nothing to play")
	}
	if what != "genre" {
		name = ""
	}
	songs, err := cl.RandomSongs(ctx, shuffleSongs, name)
	if err != nil {
		return d.errToast(err)
	}
	return d.playNow(songs, "Nothing to play")
}

// playArtist plays every album of an artist, in order or shuffled. The reply asks only for the
// artist; the albums' songs are fetched behind it, as many albums don't fit one reply's time. A
// later play, or Sign Out, in that time wins: this one then neither starts nor leaves its error.
func (d *daemon) playArtist(ctx context.Context, cl *subsonic.Client, id string, shuffle bool) q2spot.Reply {
	it, err := d.item(ctx, cl, selection.KindArtist, id)
	if err != nil {
		return d.errToast(err)
	}
	if len(it.albums) == 0 {
		return q2spot.Fail(q2spot.Unavailable, "Nothing to play")
	}
	seq := d.nextPlay()
	d.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), artistPlayIn)
		defer cancel()
		var songs []subsonic.Song
		var err error
		for _, a := range it.albums {
			if !d.latestPlay(seq) {
				return
			}
			var s []subsonic.Song
			if _, s, err = getAlbum(ctx, cl, a.ID); err != nil {
				break
			}
			songs = append(songs, s...)
		}
		if err == nil && len(songs) == 0 {
			err = errors.New("no songs")
		}
		if shuffle {
			rand.Shuffle(len(songs), func(i, j int) { songs[i], songs[j] = songs[j], songs[i] })
		}
		d.playMu.Lock()
		defer d.playMu.Unlock()
		switch {
		case !d.latestPlay(seq):
		case err != nil: // shown on the root, as a stream that gave up is
			d.log.Printf("error: %v", err)
			d.stream.giveUp(&streamErr{title: it.name, at: time.Now(), err: err}) // in Diagnostics too
		default:
			d.stream.start(songs, 0)
		}
	})
	if shuffle {
		return q2spot.Reply{Message: "Shuffling " + it.name}
	}
	return q2spot.Reply{Message: "Playing " + it.name}
}
