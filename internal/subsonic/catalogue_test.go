package subsonic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"testing"
)

// recorder answers every request with body and keeps each one's op and query.
func recorder(t *testing.T, body string) (*Client, *[]string, *[]url.Values) {
	t.Helper()
	var ops []string
	var qs []url.Values
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ops, qs = append(ops, path.Base(r.URL.Path)), append(qs, r.URL.Query())
		w.Write([]byte(`{"subsonic-response":{"status":"ok","version":"1.16.1"` + body + `}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c, &ops, &qs
}

// The walk's pages carry what the Server Library keeps besides a song's tags.
func TestSongInfos(t *testing.T) {
	c, _, qs := recorder(t, `,"searchResult3":{"song":[{"id":"s1","title":"T","album":"A","albumId":"a1",
		"artist":"Ar","year":2001,"discNumber":1,"track":3,"duration":200,"size":4000000,"suffix":"flac",
		"genre":"Rock","musicBrainzId":"mb-1","userRating":4,"starred":"2026-11-01T10:00:00Z"}]}`)
	got, err := c.SongInfos(context.Background(), 500, 500)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	s := got[0]
	if s.ID != "s1" || s.Genre != "Rock" || s.MBID != "mb-1" || s.Rating != 4 || s.Starred != "2026-11-01T10:00:00Z" ||
		s.Size != 4000000 || s.Duration != 200 || s.AlbumArtist != "Ar" {
		t.Fatalf("%+v", s)
	}
	if q := (*qs)[0]; q.Get("query") != `""` || q.Get("songOffset") != "500" || q.Get("songCount") != "500" {
		t.Fatal(q)
	}
}

func TestStarredSongsAndSongInfo(t *testing.T) {
	c, ops, _ := recorder(t, `,"starred2":{"song":[{"id":"s1","starred":"2026-11-01T10:00:00Z"}]},
		"song":{"id":"s2","userRating":2}`)
	st, err := c.StarredSongs(context.Background())
	if err != nil || len(st) != 1 || st[0].Starred == "" {
		t.Fatal(st, err)
	}
	si, err := c.SongInfo(context.Background(), "s2")
	if err != nil || si.ID != "s2" || si.Rating != 2 || si.Starred != "" {
		t.Fatal(si, err)
	}
	if (*ops)[0] != "getStarred2" || (*ops)[1] != "getSong" {
		t.Fatal(*ops)
	}
}

func TestIndexesModified(t *testing.T) {
	c, _, qs := recorder(t, `,"indexes":{"lastModified":1700000000000,"index":[]}`)
	if ms, err := c.IndexesModified(context.Background(), 0); err != nil || ms != 1700000000000 {
		t.Fatal(ms, err)
	}
	if _, ok := (*qs)[0]["ifModifiedSince"]; ok {
		t.Fatal("ifModifiedSince sent with nothing seen")
	}
	c.IndexesModified(context.Background(), 1600000000000)
	if (*qs)[1].Get("ifModifiedSince") != "1600000000000" {
		t.Fatal((*qs)[1])
	}
}

func TestRatingCalls(t *testing.T) {
	c, ops, qs := recorder(t, ``)
	ctx := context.Background()
	if c.SetRating(ctx, "s1", 3) != nil || c.Star(ctx, "s1") != nil || c.Unstar(ctx, "s2") != nil {
		t.Fatal("failed")
	}
	want := []string{"setRating", "star", "unstar"}
	for i, op := range want {
		if (*ops)[i] != op {
			t.Fatal(*ops)
		}
	}
	if (*qs)[0].Get("id") != "s1" || (*qs)[0].Get("rating") != "3" || (*qs)[2].Get("id") != "s2" {
		t.Fatal(*qs)
	}
}

func TestPlaylistsChanged(t *testing.T) {
	c, _, _ := recorder(t, `,"playlists":{"playlist":[{"id":"p1","name":"P","songCount":2,"changed":"2026-11-02T00:00:00Z"}]}`)
	pl, err := c.Playlists(context.Background())
	if err != nil || len(pl) != 1 || pl[0].Changed != "2026-11-02T00:00:00Z" {
		t.Fatal(pl, err)
	}
}

// Only no answer is a lost server: an HTTP error or a refused sign-in is a server that is there.
func TestConnectionLost(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	refused, _ := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	srv.Close()
	if err := refused.Ping(ctx); !ConnectionLost(err) {
		t.Fatalf("refused: %v", err)
	}
	if err := serveJSON(t, failed(40)).Ping(ctx); err == nil || ConnectionLost(err) {
		t.Fatalf("wrong password: %v", err)
	}
	s500 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer s500.Close()
	c, _ := New(Creds{Server: s500.URL, User: "u", Password: "pw"}, s500.Client())
	if err := c.Ping(ctx); err == nil || ConnectionLost(err) {
		t.Fatalf("http 500: %v", err)
	}
	if ConnectionLost(nil) {
		t.Fatal("nil")
	}
}
