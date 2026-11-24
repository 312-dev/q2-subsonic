package subsonic

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowLink is a TCP proxy to target whose every connection carries the server's bytes at rate
// bytes a second, queued without limit behind each other: a slow path where the server's side
// holds what it sent and the client gets it in order. Bytes the other way go at once.
type slowLink struct {
	ln    net.Listener
	mu    sync.Mutex
	conns int
}

func newSlowLink(t *testing.T, target string, rate int) *slowLink {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &slowLink{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			l.mu.Lock()
			l.conns++
			l.mu.Unlock()
			go func() { io.Copy(s, c); s.Close() }()
			queue := make(chan []byte, 1<<16)
			go func() { // the server's side: takes all it sends
				defer close(queue)
				for {
					b := make([]byte, 16<<10)
					n, err := s.Read(b)
					if n > 0 {
						queue <- b[:n]
					}
					if err != nil {
						return
					}
				}
			}()
			go func() { // the link: rate bytes a second, in order
				defer c.Close()
				for b := range queue {
					for len(b) > 0 {
						n := min(len(b), rate/20)
						if _, err := c.Write(b[:n]); err != nil {
							return
						}
						b = b[n:]
						time.Sleep(50 * time.Millisecond)
					}
				}
			}()
		}
	}()
	return l
}

func (l *slowLink) count() int { l.mu.Lock(); defer l.mu.Unlock(); return l.conns }

// A song streaming over a slow path (40 KB/s, every byte queued in order) holds megabytes queued
// for the client; an API call or the server check on the same connection waits behind them all,
// and ran out of its 5 to 10 s while the song still arrived. Calls go over a connection of their
// own, and answer in about a round trip of the slow link.
func TestAnAPICallDoesNotWaitBehindASlowStream(t *testing.T) {
	song := strings.Repeat("x", 4<<20)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stream") {
			w.Header().Set("Content-Type", "audio/flac")
			io.WriteString(w, song)
			return
		}
		io.WriteString(w, `{"subsonic-response":{"status":"ok"}}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	link := newSlowLink(t, srv.Listener.Addr().String(), 40<<10)
	c, err := New(Creds{Server: "https://" + link.ln.Addr().String(), User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil { // the connection there before the song
		t.Fatal(err)
	}
	resp, err := c.Get(context.Background(), c.PlayURL("s1", ""), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go io.Copy(io.Discard, resp.Body) // the song arrives as fast as the link lets it
	time.Sleep(time.Second)           // the server has sent it all; the link has 40 KB of it through
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("the check behind the song: %v after %v, %d connections", err, time.Since(start).Round(time.Millisecond), link.count())
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("%s: the test needs HTTP/2", resp.Proto)
	}
}
