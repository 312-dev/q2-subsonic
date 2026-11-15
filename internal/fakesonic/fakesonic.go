// Package fakesonic is an OpenSubsonic server for tests: songs, playlists, stars and ratings in
// memory, the calls the Server Library makes, and switches for a server that is down or refuses
// the sign-in.
package fakesonic

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
)

// Song is one song the server has.
type Song struct {
	ID, Title, Album, AlbumID, Artist, Suffix, Genre, MBID string
	Year, Track, Duration                                  int
	Size                                                   int64
	Rating                                                 int
	Starred                                                string
}

type Playlist struct {
	ID, Name, Changed string
	SIDs              []string
}

// Server is the fake. Lock Mu to change its fields while it serves.
type Server struct {
	*httptest.Server
	Mu        sync.Mutex
	User, Pwd string
	Songs     []Song
	Playlists []Playlist
	LastScan  string
	Count     int64 // getScanStatus's count; -1 answers getScanStatus with an error 404 (no such call)
	Modified  int64 // getIndexes' lastModified
	Down      bool  // every connection is closed unanswered
	NoEmpty   bool  // the empty query finds nothing, as on a server that doesn't take it
	Calls     map[string]int
	Writes    []string          // "setRating s1 3", "star s1", "unstar s1", "createPlaylist p1 s1,s2", "updatePlaylist p1 name=x add=s3", "deletePlaylist p1"
	Covers    map[string][]byte // getCoverArt's picture by id; an id not here is error 70
}

// New starts a TLS server with user u and password "pw".
func New() *Server {
	f := &Server{User: "u", Pwd: "pw", Calls: map[string]int{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Server) find(id string) *Song {
	for i := range f.Songs {
		if f.Songs[i].ID == id {
			return &f.Songs[i]
		}
	}
	return nil
}

func (s Song) wire() map[string]any {
	m := map[string]any{"id": s.ID, "title": s.Title, "album": s.Album, "albumId": s.AlbumID, "artist": s.Artist,
		"suffix": s.Suffix, "year": s.Year, "track": s.Track, "duration": s.Duration, "size": s.Size, "discNumber": 1}
	if s.Genre != "" {
		m["genre"] = s.Genre
	}
	if s.MBID != "" {
		m["musicBrainzId"] = s.MBID
	}
	if s.Rating > 0 {
		m["userRating"] = s.Rating
	}
	if s.Starred != "" {
		m["starred"] = s.Starred
	}
	return m
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	op := path.Base(r.URL.Path)
	f.Calls[op]++
	if f.Down {
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
	}
	q := r.URL.Query()
	ok := func(body map[string]any) {
		body["status"], body["version"] = "ok", "1.16.1"
		json.NewEncoder(w).Encode(map[string]any{"subsonic-response": body})
	}
	fail := func(code int, msg string) {
		json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "failed", "version": "1.16.1",
			"error": map[string]any{"code": code, "message": msg}}})
	}
	sum := md5.Sum([]byte(f.Pwd + q.Get("s")))
	if q.Get("u") != f.User || q.Get("t") != hex.EncodeToString(sum[:]) {
		fail(40, "Wrong username or password")
		return
	}
	switch op {
	case "ping":
		ok(map[string]any{})
	case "getScanStatus":
		if f.Count < 0 {
			w.WriteHeader(404)
			return
		}
		ok(map[string]any{"scanStatus": map[string]any{"scanning": false, "count": f.Count, "lastScan": f.LastScan}})
	case "getIndexes":
		ok(map[string]any{"indexes": map[string]any{"lastModified": f.Modified, "index": []any{}}})
	case "search3":
		off, _ := strconv.Atoi(q.Get("songOffset"))
		n, _ := strconv.Atoi(q.Get("songCount"))
		var out []any
		if q.Get("query") == `""` && !f.NoEmpty {
			for i := off; i < len(f.Songs) && i < off+n; i++ {
				out = append(out, f.Songs[i].wire())
			}
		}
		ok(map[string]any{"searchResult3": map[string]any{"song": out}})
	case "getSong":
		s := f.find(q.Get("id"))
		if s == nil {
			fail(70, "not found")
			return
		}
		ok(map[string]any{"song": s.wire()})
	case "getStarred2":
		var out []any
		for _, s := range f.Songs {
			if s.Starred != "" {
				out = append(out, s.wire())
			}
		}
		ok(map[string]any{"starred2": map[string]any{"song": out}})
	case "getPlaylists":
		var out []any
		for _, p := range f.Playlists {
			out = append(out, map[string]any{"id": p.ID, "name": p.Name, "songCount": len(p.SIDs), "changed": p.Changed})
		}
		ok(map[string]any{"playlists": map[string]any{"playlist": out}})
	case "getPlaylist":
		for _, p := range f.Playlists {
			if p.ID == q.Get("id") {
				var out []any
				for _, id := range p.SIDs {
					if s := f.find(id); s != nil {
						out = append(out, s.wire())
					}
				}
				ok(map[string]any{"playlist": map[string]any{"id": p.ID, "name": p.Name, "entry": out}})
				return
			}
		}
		fail(70, "not found")
	case "setRating", "star", "unstar":
		s := f.find(q.Get("id"))
		if s == nil {
			fail(70, "not found")
			return
		}
		switch op {
		case "setRating":
			s.Rating, _ = strconv.Atoi(q.Get("rating"))
			f.Writes = append(f.Writes, op+" "+s.ID+" "+q.Get("rating"))
		case "star":
			s.Starred = "2026-11-14T12:00:00Z"
			f.Writes = append(f.Writes, op+" "+s.ID)
		case "unstar":
			s.Starred = ""
			f.Writes = append(f.Writes, op+" "+s.ID)
		}
		ok(map[string]any{})
	case "createPlaylist", "updatePlaylist", "deletePlaylist":
		id := q.Get("playlistId")
		if op == "deletePlaylist" {
			id = q.Get("id")
		}
		i := 0
		for i < len(f.Playlists) && f.Playlists[i].ID != id {
			i++
		}
		if i == len(f.Playlists) {
			fail(70, "Playlist not found")
			return
		}
		p := &f.Playlists[i]
		p.Changed = "2026-11-15T12:00:00Z"
		switch op {
		case "createPlaylist":
			p.SIDs = append([]string(nil), q["songId"]...)
			f.Writes = append(f.Writes, op+" "+id+" "+strings.Join(q["songId"], ","))
		case "updatePlaylist":
			w := op + " " + id
			if n := q.Get("name"); n != "" {
				p.Name, w = n, w+" name="+n
			}
			if add := q["songIdToAdd"]; len(add) > 0 {
				p.SIDs, w = append(p.SIDs, add...), w+" add="+strings.Join(add, ",")
			}
			f.Writes = append(f.Writes, w)
		case "deletePlaylist":
			f.Playlists = append(f.Playlists[:i], f.Playlists[i+1:]...)
			f.Writes = append(f.Writes, op+" "+id)
		}
		ok(map[string]any{})
	case "getCoverArt":
		b, have := f.Covers[q.Get("id")]
		if !have {
			fail(70, "Artwork not found")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(b)
	default:
		w.WriteHeader(404)
	}
}

// Library is n songs, s1 to sn, of 10 albums.
func Library(n int) []Song {
	out := make([]Song, n)
	for i := range out {
		id := "s" + strconv.Itoa(i+1)
		out[i] = Song{ID: id, Title: "Title " + id, Album: "Album " + strconv.Itoa(i%10), AlbumID: "al" + strconv.Itoa(i%10),
			Artist: "Artist", Suffix: "flac", Year: 2000, Track: i/10 + 1, Duration: 180 + i%60, Size: int64(1000000 + i)}
	}
	return out
}
