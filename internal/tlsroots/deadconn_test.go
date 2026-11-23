package tlsroots

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// blackHole is a TCP proxy to target whose connections can be made to go silent, as a Wi-Fi that
// joins another network leaves the Q2's pooled connection: its source address is gone, so nothing
// it sends arrives and nothing comes back, and no reset says so. New connections still go through.
type blackHole struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []*holeConn
}

type holeConn struct {
	mu   sync.Mutex
	dead bool
}

func (h *holeConn) isDead() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.dead }

func newBlackHole(t *testing.T, target string) *blackHole {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &blackHole{ln: ln, target: target}
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
			hc := &holeConn{}
			b.mu.Lock()
			b.conns = append(b.conns, hc)
			b.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if err != nil {
						dst.Close()
						return
					}
					if hc.isDead() {
						continue // swallowed: no bytes, no reset
					}
					if _, err := dst.Write(buf[:n]); err != nil {
						src.Close()
						return
					}
				}
			}
			go pipe(s, c)
			go pipe(c, s)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return b
}

// silence makes every connection open now a black hole.
func (b *blackHole) silence() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		c.mu.Lock()
		c.dead = true
		c.mu.Unlock()
	}
	return len(b.conns)
}

func (b *blackHole) count() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.conns) }

// The drive of 2026-11-23: the Wi-Fi joined another network mid-drive, and every request for the
// next 16 minutes went to the pooled HTTP/2 connection that had died with the old address, timing
// out one by one ("http2: timeout awaiting response headers"), until the kernel gave the connection
// up. Against a server reachable on a new connection, requests made after the old one goes silent
// must reach it again within seconds.
func TestADeadPooledConnectionIsLeftWithinSeconds(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	hole := newBlackHole(t, srv.Listener.Addr().String())
	roots := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	tr := newTransport(roots, 2*time.Second)
	// the server's certificate is for 127.0.0.1, which the proxy also is
	c := &http.Client{Transport: tr}
	url := "https://" + hole.ln.Addr().String() + "/rest/ping"
	get := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Fatalf("%s: the test needs HTTP/2", resp.Proto)
		}
		return nil
	}
	if err := get(); err != nil {
		t.Fatal(err)
	}
	if n := hole.silence(); n != 1 {
		t.Fatalf("%d connections before the black hole; want 1", n)
	}
	start := time.Now()
	const within = 20 * time.Second
	var fails int
	for {
		err := get()
		if err == nil {
			break
		}
		fails++
		t.Logf("%.1fs: %v", time.Since(start).Seconds(), err)
		if time.Since(start) > within {
			t.Fatalf("%d requests failed in %s on a dead connection, %d dialled; a new one would have answered",
				fails, within, hole.count())
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("back after %.1fs, %d failed, %d connections", time.Since(start).Seconds(), fails, hole.count())
	if hole.count() < 2 {
		t.Fatalf("answered on the silenced connection")
	}
}
