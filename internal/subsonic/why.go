package subsonic

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http/httptrace"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andsuor/q2-subsonic/internal/tlsroots"
)

// A request that fails says in its log line why, in one phrase, and how far it got: whether the
// resolver file listed a name server, whether the name, the connect, the TLS handshake or the answer failed,
// and on a reused connection how long that had been idle. "context deadline exceeded" alone can't
// tell a dead pooled connection from a network gone.

// reqTrace is how far one request got (its httptrace hooks).
type reqTrace struct {
	mu     sync.Mutex
	start  time.Time
	got    bool          // a connection was had, new or reused
	reused bool          // it was a pooled one
	idle   time.Duration // how long the pooled one had been idle
	phase  string        // the step under way: "resolving", "connecting", "the TLS handshake", "sending"
	sent   time.Duration // when the request was written, from start; 0 not yet
}

func newReqTrace() *reqTrace { return &reqTrace{start: time.Now(), phase: "waiting for a connection"} }

func (t *reqTrace) set(f func()) { t.mu.Lock(); f(); t.mu.Unlock() }

func (t *reqTrace) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { t.set(func() { t.phase = "resolving" }) },
		ConnectStart:      func(string, string) { t.set(func() { t.phase = "connecting" }) },
		TLSHandshakeStart: func() { t.set(func() { t.phase = "the TLS handshake" }) },
		GotConn: func(i httptrace.GotConnInfo) {
			t.set(func() { t.got, t.reused, t.idle, t.phase = true, i.Reused, i.IdleTime, "sending" })
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			t.set(func() { t.sent, t.phase = time.Since(t.start), "waiting for the answer" })
		},
	}
}

// where is how far the request got, as "a reused connection idle 47s: sent at 0.0s, no answer
// in 10.0s".
func (t *reqTrace) where() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	took := time.Since(t.start).Round(100 * time.Millisecond)
	conn := "no connection"
	switch {
	case t.got && t.reused:
		conn = fmt.Sprintf("a reused connection idle %s", t.idle.Round(time.Second))
	case t.got:
		conn = "a new connection"
	}
	if t.sent > 0 {
		return fmt.Sprintf("%s: sent at %.1fs, no answer in %s", conn, t.sent.Seconds(), took)
	}
	return fmt.Sprintf("%s: %s after %s", conn, t.phase, took)
}

// failWhy is a failed request's reason in a few words: no name server to ask, the name,
// the connect, the TLS handshake, a connection gone silent, or no answer in time with how far the
// request got (t).
func failWhy(err error, t *reqTrace) string {
	var de *net.DNSError
	switch {
	case errors.As(err, &de):
		switch {
		case de.Err == tlsroots.ErrNoNameServer.Error():
			return "dns: the resolver file lists no name server (no lease yet, or the file is being rewritten)"
		case de.Server == "[::1]:53" || de.Server == "127.0.0.1:53":
			return fmt.Sprintf("dns: the resolver in use has no name server, the resolver file lists %d", len(tlsroots.NameServers()))
		case errors.Is(err, syscall.ENETUNREACH) || strings.Contains(de.Err, "network is unreachable"):
			return "dns: no route to the name server (the Wi-Fi has no network)"
		case de.IsTimeout:
			return "dns: the name server didn't answer"
		case de.IsNotFound:
			return "dns: the name doesn't exist"
		case strings.Contains(de.Err, "server misbehaving"):
			return "dns: the name server failed the name"
		}
		return "dns: " + de.Err
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "dial: no route to the server (the Wi-Fi has no network)"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "dial: the server refused the connection"
	case errors.Is(err, syscall.ECONNRESET):
		return "the connection was reset; " + t.where()
	case strings.Contains(err.Error(), "TLS handshake timeout"):
		return "tls: no handshake in time"
	case isCert(err):
		return "tls: the certificate"
	case errors.As(err, new(tls.AlertError)), errors.As(err, new(*tls.RecordHeaderError)):
		return "tls: the handshake failed"
	case strings.Contains(err.Error(), "http2: client connection lost"):
		return "the connection went silent (no answer to its ping); " + t.where()
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "timeout awaiting response headers"):
		return "deadline: " + t.where()
	case errors.Is(err, context.Canceled):
		return "cancelled: " + t.where()
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return "dial: " + oe.Err.Error()
	}
	return t.where()
}
