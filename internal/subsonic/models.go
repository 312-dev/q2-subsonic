package subsonic

type Song struct {
	ID, Title, Album, AlbumID, Artist, AlbumArtist string
	Year, Disc, Track, Duration                    int
	Size                                           int64
	Suffix, CoverArt                               string
	BitRate                                        int
	// The track artist's, from getSong alone ("" when the server names none): a list's songs, the
	// Entire Library's among them, neither hold it nor write it to library.json.
	ArtistID string `json:",omitempty"`
}

type Album struct {
	ID, Name, Artist string
	Year, SongCount  int
	CoverArt         string
}

type Artist struct {
	ID, Name   string
	AlbumCount int
	Index      string // the name of the server's index group it is listed in (getArtists only)
}

type Playlist struct {
	ID, Name  string
	SongCount int
	CoverArt  string
	Changed   string // when it last changed, as the server writes it (getPlaylists); "" when it doesn't say
}

// SongInfo is a song with what the Server Library keeps of it besides: its genre, MusicBrainz
// recording id, the user's rating (0 to 5) and when they starred it ("" when not starred).
type SongInfo struct {
	Song
	Genre, MBID string
	Rating      int
	Starred     string
}

type Genre struct {
	Name       string `json:"value"`
	AlbumCount int    `json:"albumCount"`
	SongCount  int    `json:"songCount"`
}

// wire shapes (Subsonic / OpenSubsonic field names)
type wireSong struct {
	ID                 string `json:"id"`
	Title              string `json:"title"`
	Album              string `json:"album"`
	AlbumID            string `json:"albumId"`
	Artist             string `json:"artist"`
	DisplayAlbumArtist string `json:"displayAlbumArtist"`
	Year               int    `json:"year"`
	Disc               int    `json:"discNumber"`
	Track              int    `json:"track"`
	Duration           int    `json:"duration"`
	Size               int64  `json:"size"`
	Suffix             string `json:"suffix"`
	CoverArt           string `json:"coverArt"`
	BitRate            int    `json:"bitRate"`
	ArtistID           string `json:"artistId"`
	Genre              string `json:"genre"`
	MusicBrainzID      string `json:"musicBrainzId"`
	UserRating         int    `json:"userRating"`
	Starred            string `json:"starred"`
}

func (w wireSong) song() Song {
	aa := w.DisplayAlbumArtist
	if aa == "" {
		aa = w.Artist
	}
	return Song{w.ID, w.Title, w.Album, w.AlbumID, w.Artist, aa, w.Year, w.Disc, w.Track, w.Duration,
		w.Size, w.Suffix, w.CoverArt, w.BitRate, ""}
}

func (w wireSong) info() SongInfo {
	return SongInfo{Song: w.song(), Genre: w.Genre, MBID: w.MusicBrainzID, Rating: w.UserRating, Starred: w.Starred}
}

func infos(ws []wireSong) []SongInfo {
	out := make([]SongInfo, len(ws))
	for i, w := range ws {
		out[i] = w.info()
	}
	return out
}

func songs(ws []wireSong) []Song {
	out := make([]Song, len(ws))
	for i, w := range ws {
		out[i] = w.song()
	}
	return out
}

type wireAlbum struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Artist    string `json:"artist"`
	Year      int    `json:"year"`
	SongCount int    `json:"songCount"`
	CoverArt  string `json:"coverArt"`
}

func (w wireAlbum) album() Album { return Album(w) }

func albums(ws []wireAlbum) []Album {
	out := make([]Album, len(ws))
	for i, w := range ws {
		out[i] = w.album()
	}
	return out
}

type wireArtist struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	AlbumCount int    `json:"albumCount"`
}

func (w wireArtist) artist() Artist {
	return Artist{ID: w.ID, Name: w.Name, AlbumCount: w.AlbumCount}
}

func artists(ws []wireArtist) []Artist {
	out := make([]Artist, len(ws))
	for i, w := range ws {
		out[i] = w.artist()
	}
	return out
}
