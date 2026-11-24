package subsonic

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/andsuor/q2-subsonic/internal/applog"
)

// A ping that times out says how far it got: a pooled connection that the server no longer answers
// on is told apart from a network with no route or a name that didn't resolve.
func TestAFailedRequestsLogLineSaysHowFarItGot(t *testing.T) {
	var hang atomic.Bool
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			<-release // the request arrived; no answer comes
			return
		}
		w.Write([]byte(`{"subsonic-response":{"status":"ok"}}`))
	}))
	defer srv.Close()
	defer close(release)
	c, err := New(Creds{Server: srv.URL, User: "u", Password: "pw"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/log"
	c.Log = applog.Open(path, 1<<20)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	hang.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx); err == nil {
		t.Fatal("answered")
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "context deadline exceeded") || !strings.Contains(last, "[deadline: a reused connection idle") ||
		!strings.Contains(last, "no answer in") {
		t.Fatalf("log line %q; want the deadline with a reused connection, sent, and no answer", last)
	}
}

func TestFailWhyNamesTheStep(t *testing.T) {
	tr := newReqTrace()
	dns := func(server, msg string, timeout, notFound bool) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: msg, Name: "music.example", Server: server,
			IsTimeout: timeout, IsNotFound: notFound}}
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{dns("[::1]:53", "dial udp [::1]:53: socket: address family not supported by protocol", false, false), "dns: no name server"},
		{dns("1.0.0.1:53", "dial udp 1.0.0.1:53: connect: network is unreachable", false, false), "dns: no route"},
		{dns("10.16.0.1:53", "read udp 10.16.0.4:45325->10.16.0.1:53: i/o timeout", true, false), "dns: the name server didn't answer"},
		{dns("10.16.0.1:53", "server misbehaving", false, false), "dns: the name server failed the name"},
		{dns("10.16.0.1:53", "no such host", false, true), "dns: the name doesn't exist"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ENETUNREACH}}, "dial: no route to the server"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}, "dial: the server refused"},
		{errors.New("net/http: TLS handshake timeout"), "tls: no handshake in time"},
		{errors.New("http2: client connection lost"), "the connection went silent"},
		{context.DeadlineExceeded, "deadline: no connection: waiting for a connection after"},
	} {
		if got := failWhy(c.err, tr); !strings.HasPrefix(got, c.want) {
			t.Errorf("%v: %q; want it to start %q", c.err, got, c.want)
		}
	}
}
