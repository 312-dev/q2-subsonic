package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/selection"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// An artist's page for firmware with "artist" (q2-coda's docs/oled-design.md#artist-page): what
// the page's header and drill-down rows need, as rows the firmware reads and older themes show as
// they are. Without the word the page is as before.

const similarArtists = 20 // similar artists asked for

// artistMoreIn is how long an artist's page waits for its Top Songs and similar artists (on
// Navidrome each may ask Last.fm) before it is sent without what is missing. A var for the tests.
var artistMoreIn = 5 * time.Second

// artistRows are an artist page's rows after Radio, for firmware with "artist", and the albums
// to list after them: an info row "about" (title: the counts, "4 albums · 3 singles · 62 songs";
// subtitle: the genre, "" when unknown), Top Songs unless the server says it has none, then
// Singles & EPs, Similar Artists and All Songs, each only when it has something. albums are all
// the artist's releases; the singles and EPs among them are listed by Singles & EPs instead.
func (d *daemon) artistRows(ctx context.Context, cl *subsonic.Client, id string, albums []subsonic.Album) ([]q2spot.Row, []subsonic.Album) {
	key := selection.Key(selection.KindArtist, id)
	full, singles := releases(albums)
	songs := 0
	for _, a := range albums {
		songs += a.SongCount
	}
	var counts []string
	for _, c := range []struct {
		n   int
		one string
	}{{len(full), "album"}, {len(singles), "single"}, {songs, "song"}} {
		if c.n > 0 {
			counts = append(counts, plural(c.n, c.one))
		}
	}
	rows := []q2spot.Row{{Kind: "info", URI: "about", Title: join(counts...), Subtitle: artistGenre(albums)}}
	top, similar := d.artistMore(ctx, cl, id)
	if top {
		rows = append(rows, list("top", "Top Songs", "", key))
	}
	if len(singles) > 0 {
		rows = append(rows, list("singles", "Singles & EPs", plural(len(singles), "release"), key))
	}
	if similar > 0 {
		rows = append(rows, list("similar", "Similar Artists", plural(similar, "artist"), key))
	}
	if songs > 0 {
		rows = append(rows, list("songs", "All Songs", plural(songs, "song"), key))
	}
	return rows, full
}

// artistMore asks, side by side and each within artistMoreIn, whether the artist has Top Songs
// and how many similar artists: top is false only when the server says there are none (or has no
// Top Songs); a call that fails or is late counts no similar artists, and is asked again next time.
func (d *daemon) artistMore(ctx context.Context, cl *subsonic.Client, id string) (top bool, similar int) {
	call := cl.Within(artistMoreIn)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		songs, err := d.topSongs(ctx, cl, call, id)
		top = len(songs) > 0 || err != nil && !subsonic.Unsupported(err) && !errors.Is(err, subsonic.ErrNotFound)
	}()
	ars, _ := d.similar(ctx, cl, call, id)
	wg.Wait()
	return top, len(ars)
}

// similar is the artist's similar artists on the server, kept as their page keeps them.
func (d *daemon) similar(ctx context.Context, cl, call *subsonic.Client, id string) ([]subsonic.Artist, error) {
	return cached(d, cl, "similar:"+id, func() ([]subsonic.Artist, error) {
		return call.SimilarArtists(ctx, id, similarArtists)
	})
}

// releases splits an artist's releases into albums and singles (singles and EPs), in their order.
func releases(albums []subsonic.Album) (full, singles []subsonic.Album) {
	for _, a := range albums {
		if single(a) {
			singles = append(singles, a)
		} else {
			full = append(full, a)
		}
	}
	return full, singles
}

// single is whether a release is a single or an EP by its OpenSubsonic release types: one of them
// says Single or EP and none says Album. A server that sends none makes every release an album.
func single(a subsonic.Album) bool {
	s := false
	for _, t := range a.ReleaseTypes {
		switch strings.ToLower(t) {
		case "album":
			return false
		case "single", "ep":
			s = true
		}
	}
	return s
}

// artistGenre is the genre most of the artist's releases name (the first to get there on a tie),
// "" when none names one.
func artistGenre(albums []subsonic.Album) string {
	n, best := map[string]int{}, ""
	for _, a := range albums {
		if a.Genre == "" {
			continue
		}
		if n[a.Genre]++; n[a.Genre] > n[best] {
			best = a.Genre
		}
	}
	return best
}

// year is an album row's year, "" when the server gives none.
func year(y int) string {
	if y <= 0 {
		return ""
	}
	return strconv.Itoa(y)
}

// length is a song's length as a row shows it, 4:05 or 1:02:03; "" when the server gives none.
func length(sec int) string {
	switch {
	case sec <= 0:
		return ""
	case sec >= 3600:
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// listSong is a song row of an artist's Top Songs or All Songs: v.song, or for firmware with
// "artist" (artist) its album and length before the status in the subtitle and its album's id
// after the play context ("album:<id>", for the album's cover), which play leaves out.
func (v view) listSong(s subsonic.Song, listKey string, i int, artist bool) q2spot.Row {
	r := v.song(s, listKey, i)
	if !artist {
		return r
	}
	r.Subtitle = join(s.Album, length(s.Duration), v.songStatus(s))
	if s.AlbumID != "" && !strings.ContainsAny(s.AlbumID, " \t") {
		r.Context += " album:" + s.AlbumID
	}
	return r
}

// singlesPage is Singles & EPs: the artist's singles and EPs as album rows.
func (d *daemon) singlesPage(ctx context.Context, cl *subsonic.Client, arg, cursor string) q2spot.Reply {
	_, id := parseKey(arg)
	it, err := d.item(ctx, cl, selection.KindArtist, id)
	if err != nil {
		return d.listErr(err, cursor)
	}
	_, singles := releases(it.albums)
	v := d.view()
	var rows []q2spot.Row
	for _, a := range singles {
		rows = append(rows, v.item(selection.KindAlbum, a.ID, a.Name, year(a.Year), a.SongCount))
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("No singles or EPs")}
	}
	return d.page(rows, cursor)
}

// similarPage is Similar Artists: artist rows, each opening that artist's page.
func (d *daemon) similarPage(ctx context.Context, cl *subsonic.Client, arg, cursor string) q2spot.Reply {
	_, id := parseKey(arg)
	ars, err := d.similar(ctx, cl, cl, id)
	switch {
	case subsonic.Unsupported(err):
		return q2spot.Reply{Rows: []q2spot.Row{info(notHere)}}
	case errors.Is(err, subsonic.ErrNotFound):
		ars = nil
	case err != nil:
		return d.listErr(err, cursor)
	}
	v := d.view()
	var rows []q2spot.Row
	for _, a := range ars {
		rows = append(rows, v.item(selection.KindArtist, a.ID, a.Name, plural(a.AlbumCount, "album"), 0))
	}
	if len(rows) == 0 {
		rows = []q2spot.Row{info("No similar artists")}
	}
	return d.page(rows, cursor)
}

// songsPage is All Songs: every song of the artist's releases, release by release in the server's
// order; a song pressed plays on through them.
func (d *daemon) songsPage(ctx context.Context, cl *subsonic.Client, arg, cursor string) q2spot.Reply {
	_, id := parseKey(arg)
	key := "songs:" + id
	songs, err := cached(d, cl, key, func() ([]subsonic.Song, error) {
		songs, _, err := d.songsOf(ctx, cl, selection.KindArtist, id)
		return songs, err
	})
	if err != nil {
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
		rows = []q2spot.Row{info("No songs")}
	}
	return d.page(rows, cursor)
}
