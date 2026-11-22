package tlsroots

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// What a song's requests cost in connections and handshakes against a server that offers HTTP/2 (as
// a proxy in front of Navidrome does): the download, the playing-now report beside it, a download
// cancelled mid-body (a skip) and the next song's. Each new TLS connection is a full handshake on the
// Q2's slow CPU unless a session is resumed.
func TestASongsRequestsReuseOneConnection(t *testing.T) {
	big := strings.Repeat("x", 4<<20)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			io.WriteString(w, big)
			return
		}
		io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	var conns, full atomic.Int64
	var counted sync.Map
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
		if s != http.StateActive {
			return
		}
		if _, seen := counted.LoadOrStore(c, true); !seen {
			if tc, ok := c.(*tls.Conn); ok && !tc.ConnectionState().DidResume {
				full.Add(1)
			}
		}
	}
	srv.StartTLS()
	defer srv.Close()
	roots := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c := &http.Client{Transport: newTransport(roots, 5*time.Second)}
	get := func(path string, cancelAfter int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if cancelAfter > 0 {
			io.CopyN(io.Discard, resp.Body, int64(cancelAfter))
			cancel()
		} else {
			io.Copy(io.Discard, resp.Body)
		}
		resp.Body.Close()
	}
	get("/big", 64<<10) // a song skipped while it arrives
	get("/now", 0)      // the playing-now report
	get("/big", 0)      // the next song
	get("/ping", 0)
	t.Logf("%d connections, %d full handshakes", conns.Load(), full.Load())
	if conns.Load() != 1 {
		t.Errorf("%d connections for one song's requests; one would do", conns.Load())
	}
}

// A server with HTTP/1.1 alone: a download cancelled mid-body closes its connection, and the next
// request's new one resumes the TLS session rather than a full handshake.
func TestANewConnectionResumesTheSession(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 4<<20))
	}))
	var conns, resumed atomic.Int64
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.TLS = &tls.Config{VerifyConnection: func(cs tls.ConnectionState) error {
		if cs.DidResume {
			resumed.Add(1)
		}
		return nil
	}}
	srv.StartTLS()
	defer srv.Close()
	roots := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c := &http.Client{Transport: newTransport(roots, 5*time.Second)}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.CopyN(io.Discard, resp.Body, 64<<10)
		cancel()
		resp.Body.Close()
	}
	if conns.Load() != 2 || resumed.Load() != 1 {
		t.Fatalf("%d connections, %d resumed; the second should resume", conns.Load(), resumed.Load())
	}
}
