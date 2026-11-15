package fw

import (
	"os"
	"strings"
)

// Caps represents capabilities from the firmware.
type Caps struct {
	Playlist bool
	Progress bool
	Library  bool
	// Words of Q2 Pod 1.5.9 and later. "game" and "audio" are not for this app.
	Pages, Goto, Sleep, Sections, Art, Window, Sort bool
	// The Server Library's words: the firmware reads a catalogue and keeps hearts in step with the
	// server (q2-coda's docs/server-library.md). The app writes neither's files without its word.
	Catalogue, Ratings bool
	// "artist": the firmware draws an artist's page from the rows the app adds with it (counts and
	// genre, Singles & EPs, Similar Artists, All Songs, and songs' album, length and album id).
	Artist bool
	// "album": the firmware draws an album's page from the rows the app adds with it (Shuffle, the
	// album's artist, year, song count and running time, and each song's length).
	Album bool
}

// ReadCaps reads the firmware capabilities from path. A missing file gives all false.
// The file format is space-separated words; unknown words are ignored.
// Trailing newline/whitespace is tolerated.
func ReadCaps(path string) Caps {
	data, err := os.ReadFile(path)
	if err != nil {
		return Caps{}
	}

	words := strings.Fields(string(data))
	caps := Caps{}
	for _, word := range words {
		switch word {
		case "playlist":
			caps.Playlist = true
		case "progress":
			caps.Progress = true
		case "library":
			caps.Library = true
		case "pages":
			caps.Pages = true
		case "goto":
			caps.Goto = true
		case "sleep":
			caps.Sleep = true
		case "sections":
			caps.Sections = true
		case "art":
			caps.Art = true
		case "window":
			caps.Window = true
		case "sort":
			caps.Sort = true
		case "catalogue":
			caps.Catalogue = true
		case "ratings":
			caps.Ratings = true
		case "artist":
			caps.Artist = true
		case "album":
			caps.Album = true
		}
	}
	return caps
}
