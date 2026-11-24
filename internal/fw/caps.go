package fw

import (
	"os"
	"strings"
)

// Caps represents capabilities from the firmware.
type Caps struct {
	Playlist bool
	Progress bool
	// "status": a reply led by an info row "status" is drawn as one centred stack (Sync's states).
	Status  bool
	Library bool
	// Words of Q2 Pod 1.5.9 and later. "game" and "audio" are not for this app.
	Pages, Goto, Sleep, Sections, Art, Window, Sort bool
	// The Server Library's words: the firmware reads a catalogue and keeps stars and likes in step
	// with the server (q2-coda's docs/server-library.md). The app writes neither's files without its word.
	Catalogue, Ratings bool
	// "libsource": the firmware says in /tmp/q2-addon.library whether its Library shows the server's
	// songs ("server" or "local"); an older firmware's Streaming Songs switch could say "local", a
	// firmware with no such switch always says "server". The app pauses on "local".
	Libsource bool
	// "signin": a sign-in action's row opens the QR page itself once its refresh makes it a remote
	// row, the page draws the state file's step (signin.go), and while it is open the firmware keeps
	// /tmp/q2-<id>.qrpage, so the app knows the page was left without timing the menu's refreshes.
	Signin bool
	// "artist": the firmware draws an artist's page from the rows the app adds with it (counts and
	// genre, Singles & EPs, Similar Artists, All Songs, and songs' album, length and album id).
	Artist bool
	// "album": the firmware draws an album's page from the rows the app adds with it (Shuffle, the
	// album's artist, year, song count and running time, and each song's length).
	Album bool
	// "playlists": the firmware writes playlists.out when a server playlist changes on the Q2 (its
	// songs, its name, its deletion), which the app sends on to the server.
	Playlists bool
	// Settings > Storage (q2-coda's docs/internals.md#storage): the firmware reads storage.tsv and
	// writes storage.out. The app writes neither without the word.
	Storage bool
	// "radio": Now Playing's Radio asks the app for a song radio in /tmp/q2-<id>.radio, and lights
	// its pill while the state file says radio= (q2-coda's docs/internals.md#radio).
	Radio bool
	// "ahead": the firmware writes /tmp/q2-<id>.ahead, the songs after the one playing in its play
	// order and how many to fetch (Download Ahead), and reads /tmp/q2-<id>.stream, how far each one
	// has come. "grow": its player plays a song while it arrives, so the app writes it in place
	// rather than as a .part (q2-coda's docs/server-library.md#download-ahead-and-the-streaming-state).
	Ahead, Grow bool
	// "holdmenu": a row whose hold op is "hold" has it sent when Centre, a finger or Play/Pause is
	// held on it, and the reply's action rows are its menu; the firmware queues a song the app
	// names in /tmp/q2-<id>.queue for Play Next or Play Last (q2-coda's docs/internals.md#card-apps).
	HoldMenu bool
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
		case "status":
			caps.Status = true
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
		case "signin":
			caps.Signin = true
		case "libsource":
			caps.Libsource = true
		case "artist":
			caps.Artist = true
		case "album":
			caps.Album = true
		case "playlists":
			caps.Playlists = true
		case "storage":
			caps.Storage = true
		case "radio":
			caps.Radio = true
		case "ahead":
			caps.Ahead = true
		case "grow":
			caps.Grow = true
		case "holdmenu":
			caps.HoldMenu = true
		}
	}
	return caps
}
