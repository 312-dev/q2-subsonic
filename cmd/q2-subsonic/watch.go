package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net/http/httptrace"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// watchNames sends on kick (without blocking) whenever a file of dir named in names is written and
// closed or renamed into place, as the firmware writes .catplay: so a press is answered at once, not
// at the next of the app's 1 s ticks (the drive of 2026-11-22: about 4 s from the press to the
// server's first byte). It blocks in read, so it adds no wakeup while nothing is written; without
// inotify it returns and the tick alone reads the files, as before.
func watchNames(dir string, names []string, kick chan<- struct{}, on *atomic.Bool) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, dir, syscall.IN_CLOSE_WRITE|syscall.IN_MOVED_TO); err != nil {
		return
	}
	if on != nil {
		on.Store(true) // kicks will come: a reader may wait longer between its own looks
		defer on.Store(false)
	}
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil || n <= 0 {
			if err == syscall.EINTR {
				continue
			}
			return
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			name := buf[off+syscall.SizeofInotifyEvent : off+syscall.SizeofInotifyEvent+int(ev.Len)]
			name = bytes.TrimRight(name, "\x00")
			off += syscall.SizeofInotifyEvent + int(ev.Len)
			for _, want := range names {
				if string(name) == want {
					select {
					case kick <- struct{}{}:
					default:
					}
				}
			}
		}
	}
}

// ticks runs fn every d, and at once on a kick (watchNames), on one goroutine.
func ticks(d time.Duration, kick <-chan struct{}, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-kick:
		}
		fn()
	}
}

// pressNames are the firmware's requests in tmp answered at a kick: a press that starts a list, and
// a cover the car waits for.
func (d *daemon) pressNames() (string, []string) {
	return d.tmp, []string{filepath.Base(d.catplayPath()), filepath.Base(d.radioPath()),
		filepath.Base(d.coverAskPath()), filepath.Base(d.coverAskIapPath())}
}

// getTrace times a stream request's steps (net/http/httptrace), for the line log.txt gets at its
// headers: whether the connection was reused, then DNS, the TCP connect, the TLS handshake and the
// wait for the headers, so a slow start splits into the network's part and the server's.
type getTrace struct {
	mu                                               sync.Mutex
	start, dns0, dns1, tcp0, tcp1, tls0, tls1, wrote time.Time
	reused                                           bool
}

func (g *getTrace) at(p *time.Time) {
	g.mu.Lock()
	*p = time.Now()
	g.mu.Unlock()
}

func (g *getTrace) trace() *httptrace.ClientTrace {
	g.start = time.Now()
	return &httptrace.ClientTrace{
		GotConn:           func(i httptrace.GotConnInfo) { g.mu.Lock(); g.reused = i.Reused; g.mu.Unlock() },
		DNSStart:          func(httptrace.DNSStartInfo) { g.at(&g.dns0) },
		DNSDone:           func(httptrace.DNSDoneInfo) { g.at(&g.dns1) },
		ConnectStart:      func(string, string) { g.at(&g.tcp0) },
		ConnectDone:       func(string, string, error) { g.at(&g.tcp1) },
		TLSHandshakeStart: func() { g.at(&g.tls0) },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { g.at(&g.tls1) },
		WroteRequest:      func(httptrace.WroteRequestInfo) { g.at(&g.wrote) },
	}
}

// line is the trace as log.txt says it, headers being when the response's headers came.
func (g *getTrace) line(headers time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ms := func(a, b time.Time) int64 {
		if a.IsZero() || b.IsZero() {
			return -1
		}
		return b.Sub(a).Milliseconds()
	}
	return fmt.Sprintf("request: connection reused %v, dns %d ms, connect %d ms, tls %d ms, headers %d ms after the request",
		g.reused, ms(g.dns0, g.dns1), ms(g.tcp0, g.tcp1), ms(g.tls0, g.tls1), ms(g.wrote, headers))
}

// watched are the firmware's files in tmp whose change the stream's run loop answers at once.
func (s *streamer) watched() []string {
	var out []string
	for _, p := range []string{s.playingFile, s.aheadFile, s.retryFile} {
		if p != "" {
			out = append(out, filepath.Base(p))
		}
	}
	return out
}
