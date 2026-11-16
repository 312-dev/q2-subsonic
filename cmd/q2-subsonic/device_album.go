package main

import (
	"cmp"
	"math/rand/v2"
	"slices"

	"github.com/andsuor/q2-subsonic/internal/q2spot"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// An album's page for firmware with "album" (q2-coda's docs/oled-design.md#album-page): what the
// page's header and track rows need, as rows the firmware reads and older themes show as they
// are. Without the word the page is as before.

// albumRows are an album page's rows after Play, for firmware with "album": Shuffle (the album's
// songs in random order), then an info row "about" whose title is the album's artist and whose
// subtitle is "2015 · 18 songs · 1h12m", each part left out when unknown.
func albumRows(key string, a subsonic.Album, songs []subsonic.Song) []q2spot.Row {
	y, sec := a.Year, 0
	for _, s := range songs {
		if y <= 0 {
			y = s.Year
		}
		sec += max(s.Duration, 0)
	}
	artist := a.Artist
	if artist == "" && len(songs) > 0 {
		artist = cmp.Or(songs[0].AlbumArtist, songs[0].Artist)
	}
	return []q2spot.Row{act("shuffle", "Shuffle", key),
		{Kind: "info", URI: "about", Title: artist, Subtitle: join(year(y), plural(len(songs), "song"), span(sec))}}
}

// span is an album's running time as its page's header says it, in the firmware's time units
// (minutes): "1h12m" or "38 min"; "" when the server gives no lengths.
func span(sec int) string {
	m := (sec + 30) / 60
	switch {
	case sec <= 0:
		return ""
	case m < 1:
		return "1 min"
	}
	return minutes(m)
}

// albumSong is a song row of an album's page: v.song, with its length between the artist and the
// status for firmware with "album" (album), which draws it on the track's row.
func (v view) albumSong(s subsonic.Song, key string, i int, album bool) q2spot.Row {
	r := v.song(s, key, i)
	if album {
		r.Subtitle = join(s.Artist, length(s.Duration), v.songStatus(s))
	}
	return r
}

// shuffled is songs in a random order, in a new slice: the cached album keeps its own.
func shuffled(songs []subsonic.Song) []subsonic.Song {
	out := slices.Clone(songs)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}
