package tlsroots

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// The resolver modes of fakeLookup.
const (
	dnsAnswers = iota
	dnsFails   // the resolver answers that it can't (SERVFAIL: "server misbehaving")
	dnsHangs   // the query is lost, and the resolver waits out its 5 s
)

// fakeLookup makes the dialler's lookups answer as *mode says: example.com is 127.0.0.1.
func fakeLookup(t *testing.T, mode *atomic.Int32) {
	old, oldSlow := lookupHost, slowLookup
	t.Cleanup(func() { lookupHost, slowLookup = old, oldSlow; forget() })
	forget()
	lookupHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		switch mode.Load() {
		case dnsFails:
			return nil, &net.DNSError{Err: "server misbehaving", Name: host, Server: "10.16.0.1:53", IsTemporary: true}
		case dnsHangs:
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			return nil, &net.DNSError{Err: "i/o timeout", Name: host, Server: "10.16.0.1:53", IsTimeout: true}
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
}

// tlsServer is an HTTPS server whose certificate names example.com, and a client of it through
// newTransport, with its URL by that name.
func tlsServer(t *testing.T) (*http.Client, *http.Transport, string) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	roots := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	tr := newTransport(roots, 3*time.Second)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return &http.Client{Transport: tr}, tr, "https://example.com:" + port + "/rest/ping"
}

func get(c *http.Client, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// A network whose resolver fails the server's name ("server misbehaving") or drops the query (5 s
// a try) while the server itself answers: a new connection to a server reached before goes to the
// address it had, at once on a failure and after slowLookup on no answer, the certificate still
// checked against the name.
func TestARememberedAddressStandsInForAFailingOrSlowName(t *testing.T) {
	var mode atomic.Int32
	fakeLookup(t, &mode)
	c, tr, url := tlsServer(t)
	if err := get(c, url); err != nil {
		t.Fatal(err)
	}
	mode.Store(dnsFails)
	tr.CloseIdleConnections() // a new connection: the name is looked up again
	if err := get(c, url); err != nil {
		t.Fatalf("the name failing: %v; the address it had answers", err)
	}
	mode.Store(dnsHangs)
	tr.CloseIdleConnections()
	start := time.Now()
	if err := get(c, url); err != nil {
		t.Fatalf("the name not answering: %v; the address it had answers", err)
	}
	if took := time.Since(start); took > slowLookup+2*time.Second {
		t.Fatalf("connected in %v with the lookup hanging; want about %v", took, slowLookup)
	}
}

// A name never connected to has no stand-in: its lookup's error is the dial's, a *net.DNSError as
// Go's own dialler gives, so the request's error still says the name didn't resolve.
func TestANameNeverReachedFailsWithItsLookup(t *testing.T) {
	var mode atomic.Int32
	mode.Store(dnsFails)
	fakeLookup(t, &mode)
	c, _, url := tlsServer(t)
	err := get(c, url)
	var de *net.DNSError
	if !errors.As(err, &de) {
		t.Fatalf("got %v; want the lookup's *net.DNSError", err)
	}
}
