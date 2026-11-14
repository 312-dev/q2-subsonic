//go:build realserver

package catalogue

// A first copy measured against a real server (read-only calls only: search3, getScanStatus,
// getPlaylists, getPlaylist, getStarred2):
//
//	go test -c -tags realserver -o target/realserver.test ./internal/catalogue
//	Q2_REAL_URL=http://host:4533 U=user P=password Q2_REAL_OUT=/tmp/q2cat \
//		target/realserver.test -test.run TestRealFirstCopy -test.v
//
// The server address may be plain http (NewPlain); the user and password come from the
// environment and are never printed.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

type counting struct {
	n  atomic.Int64
	rt http.RoundTripper
}

func (c *counting) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.rt.RoundTrip(r)
}

func TestRealFirstCopy(t *testing.T) {
	base, out := os.Getenv("Q2_REAL_URL"), os.Getenv("Q2_REAL_OUT")
	if base == "" || out == "" {
		t.Skip("Q2_REAL_URL and Q2_REAL_OUT not set")
	}
	dir := filepath.Join(out, ".subsonic")
	os.RemoveAll(out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rt := &counting{rt: http.DefaultTransport}
	cl, err := subsonic.NewPlain(subsonic.Creds{Server: base, User: os.Getenv("U"), Password: os.Getenv("P")},
		&http.Client{Transport: rt, Timeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var peak uint64
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		peak = max(peak, m.HeapInuse)
	}
	const acct = "0badcafe"
	st := Store{out, filepath.Join(dir, SongsName)}

	t0 := time.Now()
	lastScan, count, _, err := cl.ScanStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hint := Hint{LastScan: lastScan, Count: count}
	res, err := st.Walk(ctx, cl, acct, hint, nil, func(int) { sample() })
	if err != nil {
		t.Fatal(err)
	}
	walk := time.Since(t0)
	pls, _, err := Refresh(ctx, cl, nil)
	if err != nil {
		t.Fatal(err)
	}
	Playlists{out, filepath.Join(dir, PlaylistsName)}.Save(acct, pls)
	starred, err := cl.StarredSongs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fetched := time.Since(t0)
	t1 := time.Now()
	seq, rows, err := Write(filepath.Join(dir, CatName), st, pls, Build{Card: out, App: "subsonic", Account: acct})
	if err != nil {
		t.Fatal(err)
	}
	sample()
	written := time.Since(t1)
	firstReqs := rt.n.Load()

	// A second connection with nothing changed: the hint, the playlists by date, the stars.
	t2 := time.Now()
	ls2, n2, _, _ := cl.ScanStatus(ctx)
	_, changed, err := Refresh(ctx, cl, pls)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := st.StarChanges(acct, starred, nil)
	again := time.Since(t2)
	secondReqs := rt.n.Load() - firstReqs
	pls2, _, _ := Refresh(ctx, cl, pls)
	_, changed3, _ := Refresh(ctx, cl, pls2)
	thirdReqs := rt.n.Load() - firstReqs - secondReqs

	mb, sized := 0, 0
	st.Each(acct, func(s Song) error {
		if s.MBID != "" {
			mb++
		}
		if s.Size > 0 {
			sized++
		}
		return nil
	})
	fi, _ := os.Stat(filepath.Join(dir, CatName))
	si, _ := os.Stat(st.Path)
	f, _ := os.Open(filepath.Join(dir, CatName))
	h := sha256.New()
	io.Copy(h, f)
	f.Close()
	f, _ = os.Open(filepath.Join(dir, CatName))
	head, _ := bufio.NewReader(f).ReadString('\n')
	f.Close()

	fmt.Printf("first copy: %d songs (%d gone, complete %v) in %d search3 requests, walk %v\n", res.Songs, res.Gone,
		res.Complete, res.Requests, walk.Round(time.Millisecond))
	fmt.Printf("playlists %d, starred %d, all fetched in %v; %d HTTP requests in all\n", len(pls), len(starred),
		fetched.Round(time.Millisecond), firstReqs)
	fmt.Printf("catalogue seq %d: %d rows, %d bytes, written in %v; store %d bytes; sha256 %x\n", seq, rows, fi.Size(),
		written.Round(time.Millisecond), si.Size(), h.Sum(nil))
	fmt.Printf("header: %q\n", head)
	fmt.Printf("musicBrainzId on %d songs, size on %d, rated or starred (ratings.in lines) %d\n", mb, sized, len(res.Changes))
	fmt.Printf("heap in use, peak sampled: %.1f MB\n", float64(peak)/(1<<20))
	fmt.Printf("second connection: hint same %v, playlists changed %v, star changes %d, %d requests, %v\n",
		ls2 == lastScan && n2 == count, changed, len(ch), secondReqs, again.Round(time.Millisecond))
	fmt.Printf("two more playlist checks: %d requests, the last changed %v\n", thirdReqs, changed3)
}
