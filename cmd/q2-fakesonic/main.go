// q2-fakesonic is internal/fakesonic as a program, for tests outside this repository: the firmware's
// seam test (q2-pod test/subsonic_seam.py) runs the real app against it. It serves a scripted
// library of -songs songs in albums of -album, every call the app makes (internal/fakesonic's, and
// stream, download, getAlbum, getAlbumList2, getSimilarSongs2, getCoverArt, scrobble and a few that
// answer empty), with Range on a song's bytes. A second listener, -ctl (a Unix socket), scripts it while it
// runs and reports what it was asked:
//
//	GET  /log                      every song request so far, JSON [{op, id, from, status}]
//	POST /cut?id=s5&at=N&hold=MS   the next request of s5 sends up to byte N, then nothing for MS ms,
//	                               then closes: a download cut mid-song
//	POST /delay?op=OP&ms=MS        every OP call answered MS ms late (getSimilarSongs2: a slow Radio)
//	POST /rate?id=s5&bps=N         s5's bytes sent at about N bytes a second, every request from now
//	                               on, in 16 KB writes (bps=0: at full speed again): a slow server
//
// Every album has a cover (getCoverArt with its album id), a grey JPEG of the size asked.
//
// With -audio FILE every song is that file's bytes, so a real player can decode it.
//
// It signs in user -user with password -password (token auth). It prints "listening <url>" once
// both listeners are up.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/fakesonic"
)

// Hit is one song request (stream or download) as the log reports it.
type Hit struct {
	Op     string `json:"op"`
	ID     string `json:"id"`
	From   int64  `json:"from"`
	Status int    `json:"status"`
}

type cut struct {
	at   int64
	hold time.Duration
}

type fake struct {
	f      *fakesonic.Server
	mu     sync.Mutex
	hits   []Hit
	cuts   map[string]cut
	delays map[string]time.Duration
	rates  map[string]int64 // bytes a second per song id (/rate)
	albums int
	per    int
	audio  []byte // every song's bytes when set (-audio), else body's pattern
}

// library is n songs s1 to sn, titled title+id, in albums al0, al1... of per songs each, in order,
// tracks 1 to per, each size bytes of FLAC.
func library(n, per int, size int64, title string) []fakesonic.Song {
	out := make([]fakesonic.Song, n)
	for i := range out {
		id, a := "s"+strconv.Itoa(i+1), i/per
		out[i] = fakesonic.Song{ID: id, Title: title + id, Album: "Album " + strconv.Itoa(a), AlbumID: "al" + strconv.Itoa(a),
			Artist: "Artist", Suffix: "flac", Year: 2000, Track: i%per + 1, Duration: 180, Size: size}
	}
	return out
}

// body is byte off of song id's file: "fLaC" then a pattern of its id, so a resumed download that
// lost or doubled a byte reads differently.
func body(id string, off int64) byte {
	if off < 4 {
		return "fLaC"[off]
	}
	return byte((off*31 + int64(len(id))*7 + int64(id[len(id)-1])) % 251)
}

func (k *fake) song(id string) (fakesonic.Song, bool) {
	k.f.Mu.Lock()
	defer k.f.Mu.Unlock()
	for _, s := range k.f.Songs {
		if s.ID == id {
			return s, true
		}
	}
	return fakesonic.Song{}, false
}

func (k *fake) ok(w http.ResponseWriter, body map[string]any) {
	body["status"], body["version"] = "ok", "1.16.1"
	json.NewEncoder(w).Encode(map[string]any{"subsonic-response": body})
}

func (k *fake) fail(w http.ResponseWriter, code int, msg string) {
	json.NewEncoder(w).Encode(map[string]any{"subsonic-response": map[string]any{"status": "failed", "version": "1.16.1",
		"error": map[string]any{"code": code, "message": msg}}})
}

func (k *fake) wires(songs []fakesonic.Song) []any {
	out := []any{}
	for _, s := range songs {
		out = append(out, s.Wire())
	}
	return out
}

// ServeHTTP answers the calls internal/fakesonic lacks and hands it the rest (its sign-in check
// included, so a wrong token fails here as there).
func (k *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimSuffix(path.Base(r.URL.Path), ".view")
	k.mu.Lock()
	d := k.delays[op]
	k.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	q := r.URL.Query()
	switch op {
	case "stream", "download":
		k.media(w, r, op, q.Get("id"))
		return
	case "getCoverArt":
		k.cover(w, q.Get("id"), q.Get("size"))
		return
	case "getAlbum", "getAlbumList2", "getSimilarSongs2", "scrobble", "getOpenSubsonicExtensions", "getGenres",
		"getArtists", "getRandomSongs", "getTopSongs", "getLyricsBySongId", "getArtist", "getArtistInfo2":
	default:
		k.f.ServeHTTP(w, r)
		return
	}
	k.f.Mu.Lock()
	songs := append([]fakesonic.Song(nil), k.f.Songs...)
	k.f.Mu.Unlock()
	switch op {
	case "getAlbum":
		var in []fakesonic.Song
		for _, s := range songs {
			if s.AlbumID == q.Get("id") {
				in = append(in, s)
			}
		}
		if len(in) == 0 {
			k.fail(w, 70, "Album not found")
			return
		}
		k.ok(w, map[string]any{"album": map[string]any{"id": in[0].AlbumID, "name": in[0].Album, "artist": in[0].Artist,
			"songCount": len(in), "duration": 180 * len(in), "song": k.wires(in)}})
	case "getAlbumList2":
		off, _ := strconv.Atoi(q.Get("offset"))
		n, _ := strconv.Atoi(q.Get("size"))
		var out []any
		for a := off; a < k.albums && a < off+max(n, 1); a++ {
			s := songs[a*k.per]
			out = append(out, map[string]any{"id": s.AlbumID, "name": s.Album, "artist": s.Artist, "songCount": min(k.per, len(songs)-a*k.per)})
		}
		k.ok(w, map[string]any{"albumList2": map[string]any{"album": out}})
	case "getSimilarSongs2":
		// the songs after the seed in the library's order, wrapping, never the seed
		n, _ := strconv.Atoi(q.Get("count"))
		at := 0
		for i, s := range songs {
			if s.ID == q.Get("id") {
				at = i
			}
		}
		var out []fakesonic.Song
		for i := 1; i < len(songs) && len(out) < max(n, 1); i++ {
			out = append(out, songs[(at+i)%len(songs)])
		}
		k.ok(w, map[string]any{"similarSongs2": map[string]any{"song": k.wires(out)}})
	case "getOpenSubsonicExtensions":
		k.ok(w, map[string]any{"openSubsonicExtensions": []any{}})
	case "getLyricsBySongId":
		k.fail(w, 70, "No lyrics")
	case "getArtist", "getArtistInfo2":
		k.fail(w, 70, "Artist not found")
	default: // scrobble, getGenres, getArtists, getRandomSongs, getTopSongs: an empty answer
		k.ok(w, map[string]any{})
	}
}

// cover answers getCoverArt for an album id (al0, al1...): a grey JPEG of the size asked, 160 px
// without one; any other id is error 70.
func (k *fake) cover(w http.ResponseWriter, id, size string) {
	a, err := strconv.Atoi(strings.TrimPrefix(id, "al"))
	if !strings.HasPrefix(id, "al") || err != nil || a < 0 || a >= k.albums {
		k.fail(w, 70, "Artwork not found")
		return
	}
	px, err := strconv.Atoi(size)
	if err != nil || px < 1 || px > 1024 {
		px = 160
	}
	img := image.NewGray(image.Rect(0, 0, px, px))
	for i := range img.Pix {
		img.Pix[i] = byte(a * 37)
	}
	w.Header().Set("Content-Type", "image/jpeg")
	jpeg.Encode(w, img, nil)
}

// media sends song id's bytes from the Range asked, or holds and cuts them as a /cut said.
func (k *fake) media(w http.ResponseWriter, r *http.Request, op, id string) {
	s, have := k.song(id)
	var from int64
	if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
		from, _ = strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"), 10, 64)
	}
	status := 200
	if !have {
		status = 404
	} else if from > 0 {
		status = 206
	}
	k.mu.Lock()
	k.hits = append(k.hits, Hit{Op: op, ID: id, From: from, Status: status})
	c, cutting := k.cuts[id]
	delete(k.cuts, id)
	bps := k.rates[id]
	k.mu.Unlock()
	if !have {
		k.fail(w, 70, "Song not found")
		return
	}
	w.Header().Set("Content-Type", "audio/flac")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(s.Size-from, 10))
	if from > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, s.Size-1, s.Size))
	}
	w.WriteHeader(status)
	end := s.Size
	if cutting && c.at >= from {
		end = min(end, c.at)
	}
	chunk := 32 << 10
	if bps > 0 {
		chunk = 16 << 10
	}
	began, sent := time.Now(), int64(0)
	buf := make([]byte, 0, chunk)
	for off := from; off < end; off++ {
		if k.audio != nil {
			buf = append(buf, k.audio[off])
		} else {
			buf = append(buf, body(id, off))
		}
		if len(buf) == cap(buf) || off == end-1 {
			if bps > 0 { // paced: this chunk leaves when the rate allows it
				time.Sleep(time.Until(began.Add(time.Duration(sent * int64(time.Second) / bps))))
			}
			if _, err := w.Write(buf); err != nil {
				return
			}
			if bps > 0 {
				w.(http.Flusher).Flush()
			}
			sent += int64(len(buf))
			buf = buf[:0]
		}
	}
	if !cutting {
		return
	}
	w.(http.Flusher).Flush()
	time.Sleep(c.hold)
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

// control is the -ctl socket's handler.
func (k *fake) control() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		json.NewEncoder(w).Encode(append([]Hit{}, k.hits...))
	})
	m.HandleFunc("/cut", func(w http.ResponseWriter, r *http.Request) {
		at, err1 := strconv.ParseInt(r.FormValue("at"), 10, 64)
		hold, err2 := strconv.Atoi(r.FormValue("hold"))
		if err1 != nil || err2 != nil || r.FormValue("id") == "" {
			http.Error(w, "cut needs id, at and hold", 400)
			return
		}
		k.mu.Lock()
		k.cuts[r.FormValue("id")] = cut{at: at, hold: time.Duration(hold) * time.Millisecond}
		k.mu.Unlock()
	})
	m.HandleFunc("/delay", func(w http.ResponseWriter, r *http.Request) {
		ms, err := strconv.Atoi(r.FormValue("ms"))
		if err != nil || r.FormValue("op") == "" {
			http.Error(w, "delay needs op and ms", 400)
			return
		}
		k.mu.Lock()
		k.delays[r.FormValue("op")] = time.Duration(ms) * time.Millisecond
		k.mu.Unlock()
	})
	m.HandleFunc("/rate", func(w http.ResponseWriter, r *http.Request) {
		bps, err := strconv.ParseInt(r.FormValue("bps"), 10, 64)
		if err != nil || bps < 0 || r.FormValue("id") == "" {
			http.Error(w, "rate needs id and bps", 400)
			return
		}
		k.mu.Lock()
		k.rates[r.FormValue("id")] = bps
		k.mu.Unlock()
	})
	return m
}

func newFake(n, per int, size int64, title, user, pwd string) *fake {
	f := fakesonic.Unstarted()
	f.User, f.Pwd, f.Songs, f.Count, f.LastScan = user, pwd, library(n, per, size, title), int64(n), "2026-11-20T12:00:00Z"
	return &fake{f: f, cuts: map[string]cut{}, delays: map[string]time.Duration{}, rates: map[string]int64{}, albums: (n + per - 1) / per, per: per}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "the server's address")
	ctl := flag.String("ctl", "", "the control socket's path (none without it)")
	n := flag.Int("songs", 30, "songs in the library")
	per := flag.Int("album", 10, "songs to an album")
	size := flag.Int64("size", 64<<10, "each song's bytes")
	audio := flag.String("audio", "", "a file every song is, byte for byte (a real FLAC, for a player that decodes it); -size is then its size")
	title := flag.String("title", "Title ", "each song's title, before its id (a test that wants them first in a list sorts them so)")
	user := flag.String("user", "seam", "the user name it signs in")
	pwd := flag.String("password", "seam", "that user's password")
	flag.Parse()
	if *n < 1 || *per < 1 || *size < 8 {
		log.Fatal("q2-fakesonic: -songs and -album must be at least 1, -size at least 8")
	}
	var bytes []byte
	if *audio != "" {
		b, err := os.ReadFile(*audio)
		if err != nil || len(b) < 8 {
			log.Fatalf("q2-fakesonic: -audio: %v (%d bytes)", err, len(b))
		}
		bytes, *size = b, int64(len(b))
	}
	k := newFake(*n, *per, *size, *title, *user, *pwd)
	k.audio = bytes
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	if *ctl != "" {
		os.Remove(*ctl)
		c, err := net.Listen("unix", *ctl)
		if err != nil {
			log.Fatal(err)
		}
		go http.Serve(c, k.control())
	}
	out := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(out, "listening http://%s\n", l.Addr())
	out.Flush()
	log.Fatal(http.Serve(l, k))
}
