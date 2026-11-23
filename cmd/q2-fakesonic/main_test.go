package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func signed(base, op string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	sum := md5.Sum([]byte("seam" + "salt"))
	q.Set("u", "seam")
	q.Set("t", hex.EncodeToString(sum[:]))
	q.Set("s", "salt")
	q.Set("f", "json")
	return base + "/rest/" + op + "?" + q.Encode()
}

func get(t *testing.T, u, rng string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func serve(t *testing.T, n int) (*fake, *httptest.Server, *httptest.Server) {
	k := newFake(n, 4, 1000, "Title ", "seam", "seam")
	s, c := httptest.NewServer(k), httptest.NewServer(k.control())
	t.Cleanup(func() { s.Close(); c.Close() })
	return k, s, c
}

func TestASongIsItsSizeAndResumesWithRange(t *testing.T) {
	_, s, c := serve(t, 6)
	resp, whole := get(t, signed(s.URL, "stream", url.Values{"id": {"s2"}}), "")
	if resp.StatusCode != 200 || len(whole) != 1000 || string(whole[:4]) != "fLaC" {
		t.Fatalf("status %d, %d bytes, %q", resp.StatusCode, len(whole), whole[:4])
	}
	resp, rest := get(t, signed(s.URL, "download", url.Values{"id": {"s2"}}), "bytes=400-")
	if resp.StatusCode != 206 || string(rest) != string(whole[400:]) || resp.Header.Get("Content-Range") != "bytes 400-999/1000" {
		t.Fatalf("status %d, %d bytes, %q", resp.StatusCode, len(rest), resp.Header.Get("Content-Range"))
	}
	_, logb := get(t, c.URL+"/log", "")
	var hits []Hit
	json.Unmarshal(logb, &hits)
	if len(hits) != 2 || hits[1] != (Hit{Op: "download", ID: "s2", From: 400, Status: 206}) {
		t.Fatalf("log %+v", hits)
	}
}

func TestACutSendsItsBytesHoldsThenCloses(t *testing.T) {
	_, s, c := serve(t, 3)
	if r, _ := http.PostForm(c.URL+"/cut", url.Values{"id": {"s1"}, "at": {"300"}, "hold": {"200"}}); r.StatusCode != 200 {
		t.Fatalf("cut: %d", r.StatusCode)
	}
	t0 := time.Now()
	resp, err := http.Get(signed(s.URL, "stream", url.Values{"id": {"s1"}}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if len(b) != 300 || err == nil || time.Since(t0) < 200*time.Millisecond {
		t.Fatalf("%d bytes, err %v, after %v", len(b), err, time.Since(t0))
	}
	if _, again := get(t, signed(s.URL, "stream", url.Values{"id": {"s1"}}), ""); len(again) != 1000 {
		t.Fatalf("the cut is once: then %d bytes", len(again))
	}
	// a cut at byte 0 sends nothing at all before it closes
	http.PostForm(c.URL+"/cut", url.Values{"id": {"s2"}, "at": {"0"}, "hold": {"50"}})
	resp, err = http.Get(signed(s.URL, "stream", url.Values{"id": {"s2"}}))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(resp.Body); len(b) != 0 || err == nil {
		t.Fatalf("cut at 0: %d bytes, err %v", len(b), err)
	}
}

func TestAlbumsSimilarSongsAndADelay(t *testing.T) {
	_, s, c := serve(t, 10)
	_, b := get(t, signed(s.URL, "getAlbum", url.Values{"id": {"al1"}}), "")
	if !strings.Contains(string(b), `"id":"s5"`) || !strings.Contains(string(b), `"title":"Title s8"`) || strings.Contains(string(b), `"id":"s9"`) {
		t.Fatalf("getAlbum al1: %s", b)
	}
	http.PostForm(c.URL+"/delay", url.Values{"op": {"getSimilarSongs2"}, "ms": {"150"}})
	t0 := time.Now()
	_, b = get(t, signed(s.URL, "getSimilarSongs2", url.Values{"id": {"s10"}, "count": {"2"}}), "")
	if time.Since(t0) < 150*time.Millisecond || !strings.Contains(string(b), `"id":"s1"`) || strings.Contains(string(b), `"id":"s10"`) {
		t.Fatalf("after %v: %s", time.Since(t0), b)
	}
	_, b = get(t, signed(s.URL, "getAlbumList2", url.Values{"offset": {"2"}, "size": {"5"}}), "")
	if !strings.Contains(string(b), `"id":"al2"`) || strings.Contains(string(b), `"id":"al1"`) {
		t.Fatalf("getAlbumList2: %s", b)
	}
}

func TestAWrongTokenIsRefused(t *testing.T) {
	_, s, _ := serve(t, 1)
	_, b := get(t, s.URL+"/rest/ping?u=seam&t=00&s=salt&f=json", "")
	if !strings.Contains(string(b), `"code":40`) {
		t.Fatalf("%s", b)
	}
}

func TestAnAlbumsCoverIsAJPEGOfTheSizeAsked(t *testing.T) {
	_, s, _ := serve(t, 8)
	resp, b := get(t, signed(s.URL, "getCoverArt", url.Values{"id": {"al1"}, "size": {"512"}}), "")
	img, err := jpeg.Decode(bytes.NewReader(b))
	if resp.Header.Get("Content-Type") != "image/jpeg" || err != nil || img.Bounds().Dx() != 512 {
		t.Fatalf("%v %v", err, resp.Header)
	}
	if _, b := get(t, signed(s.URL, "getCoverArt", url.Values{"id": {"al9"}}), ""); !strings.Contains(string(b), `"code":70`) {
		t.Fatalf("no album al9: %s", b)
	}
}
