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
	// "signin": a sign-in action's row opens the QR page itself once its refresh makes it a remote
	// row, the page draws the state file's step (signin.go), and while it is open the firmware keeps
	// /tmp/q2-<id>.qrpage, so the app knows the page was left without timing the menu's refreshes.
	Signin bool
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
		case "signin":
			caps.Signin = true
		}
	}
	return caps
}
