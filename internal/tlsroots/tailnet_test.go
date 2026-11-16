package tlsroots

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeTailnet points tailnetState at a file holding state, a DNS answer per name, and a fixed clock.
func fakeTailnet(t *testing.T, state string, dns map[string]string) *time.Time {
	t.Helper()
	path := filepath.Join(t.TempDir(), "q2-tailscale.state")
	if state != "" {
		if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)
	oldState, oldLookup, oldNow := tailnetState, lookupHost, tnNow
	tailnetState, tnRead, tnProxy, tnHosts = path, time.Time{}, nil, map[string]hostAt{}
	tnNow = func() time.Time { return now }
	lookupHost = func(_ context.Context, host string) ([]netip.Addr, error) {
		a, ok := dns[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		return []netip.Addr{netip.MustParseAddr(a)}, nil
	}
	t.Cleanup(func() {
		tailnetState, lookupHost, tnNow = oldState, oldLookup, oldNow
		tnRead, tnProxy, tnHosts = time.Time{}, nil, map[string]hostAt{}
	})
	return &now
}

const running = "state=running\nname=shanling-q2\nproxy=socks5://127.0.0.1:1055\nhttpproxy=http://127.0.0.1:1056\n"

func proxyFor(t *testing.T, raw string) string {
	t.Helper()
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := TailnetProxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		return ""
	}
	return u.String()
}

func TestTailnetDestinationsGoThroughTailscaled(t *testing.T) {
	fakeTailnet(t, running, map[string]string{
		"navidrome.example.org": "100.93.15.8", // a public name pointed at a tailnet address
		"music.example.org":     "203.0.113.7",
	})
	for raw, want := range map[string]string{
		"https://macmini.tail5d173a.ts.net/rest/ping":  "socks5://127.0.0.1:1055",
		"https://MACMINI.tail5d173a.TS.NET./rest/ping": "socks5://127.0.0.1:1055",
		"http://100.93.15.8:4533/rest/ping":            "socks5://127.0.0.1:1055",
		"https://[fd7a:115c:a1e0::1]/":                 "socks5://127.0.0.1:1055",
		"https://navidrome.example.org/rest/ping":      "socks5://127.0.0.1:1055",
		"https://music.example.org/rest/ping":          "",
		"https://192.168.1.10/":                        "",
		"https://100.128.0.1/":                         "", // past 100.64.0.0/10
		"https://unresolvable.example.org/":            "",
	} {
		if got := proxyFor(t, raw); got != want {
			t.Errorf("%s: proxy %q, want %q", raw, got, want)
		}
	}
}

func TestNoProxyUnlessTailscaledIsConnected(t *testing.T) {
	for _, state := range []string{
		"",                        // no file: Tailscale off, or older firmware
		"state=starting\nname=\n", // connecting: no proxy line
		"state=needs-login\nproxy=socks5://127.0.0.1:1055\n",
		"state=running\nproxy=http://127.0.0.1:1056\n", // not SOCKS5
		"state=running\nproxy=socks5://\n",
	} {
		fakeTailnet(t, state, nil)
		if got := proxyFor(t, "https://macmini.tail5d173a.ts.net/"); got != "" {
			t.Errorf("state %q: proxy %q, want none", state, got)
		}
	}
}

func TestStateAndAnswersAreCached(t *testing.T) {
	now := fakeTailnet(t, running, map[string]string{"navidrome.example.org": "100.93.15.8"})
	lookups := 0
	inner := lookupHost
	lookupHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
		lookups++
		return inner(ctx, host)
	}
	if proxyFor(t, "https://navidrome.example.org/") == "" || proxyFor(t, "https://navidrome.example.org/") == "" {
		t.Fatal("not proxied")
	}
	if lookups != 1 {
		t.Fatalf("%d lookups, want 1 within %v", lookups, hostEvery)
	}
	if err := os.Remove(tailnetState); err != nil {
		t.Fatal(err)
	}
	if proxyFor(t, "https://macmini.tail5d173a.ts.net/") == "" {
		t.Fatal("the state is read again before stateEvery")
	}
	*now = now.Add(stateEvery)
	if proxyFor(t, "https://macmini.tail5d173a.ts.net/") != "" {
		t.Fatal("Tailscale turned off: still proxied")
	}
	if err := os.WriteFile(tailnetState, []byte(running), 0o600); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(hostEvery)
	proxyFor(t, "https://navidrome.example.org/")
	if lookups != 2 {
		t.Fatalf("%d lookups, want 2 after %v", lookups, hostEvery)
	}
}

// socks5 is a minimal SOCKS5 server (no auth, CONNECT) that records each destination and connects every
// one to target, as tailscaled's proxy would connect a tailnet name to its peer.
func socks5(t *testing.T, target string) (addr string, asked func() []string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var mu sync.Mutex
	var dests []string
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 262)
				if _, err := io.ReadFull(c, b[:2]); err != nil {
					return
				}
				io.ReadFull(c, b[:b[1]])
				c.Write([]byte{5, 0})
				io.ReadFull(c, b[:4]) // ver, CONNECT, rsv, atyp
				var host string
				switch b[3] {
				case 3:
					io.ReadFull(c, b[:1])
					n := int(b[0])
					io.ReadFull(c, b[:n])
					host = string(b[:n])
				case 1:
					io.ReadFull(c, b[:4])
					host = net.IP(b[:4]).String()
				}
				io.ReadFull(c, b[:2])
				mu.Lock()
				dests = append(dests, net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(b[:2])))))
				mu.Unlock()
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}(c)
		}
	}()
	return l.Addr().String(), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), dests...) }
}

// The real transport, end to end: a tailnet name goes to tailscaled's proxy by name, and its certificate is
// verified against the name as without the proxy; a wrong root still fails.
func TestTransportReachesATailnetServerThroughTheProxyWithItsCertificateChecked(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok "+r.Host)
	}))
	defer srv.Close()
	proxy, asked := socks5(t, srv.Listener.Addr().String())
	// httptest's certificate is for example.com: here a public name that resolves to a tailnet address
	fakeTailnet(t, "state=running\nproxy=socks5://"+proxy+"\n", map[string]string{"example.com": "100.93.15.8"})
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c := &http.Client{Transport: newTransport(roots, 5*time.Second)}
	resp, err := c.Get("https://example.com/rest/ping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok example.com" {
		t.Fatalf("body %q", body)
	}
	if got := asked(); len(got) != 1 || got[0] != "example.com:443" {
		t.Fatalf("proxy asked for %v, want [example.com:443] (the name, for tailscaled to resolve)", got)
	}
	c = &http.Client{Transport: newTransport(x509.NewCertPool(), 5*time.Second)}
	if _, err := c.Get("https://example.com/rest/ping"); err == nil {
		t.Fatal("an unknown root passed through the proxy")
	}
}

// A destination off the tailnet never touches the proxy, even while it is connected.
func TestTransportLeavesOtherServersDirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "direct") }))
	defer srv.Close()
	proxy, asked := socks5(t, "127.0.0.1:1")
	fakeTailnet(t, "state=running\nproxy=socks5://"+proxy+"\n", nil)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c := &http.Client{Transport: newTransport(roots, 5*time.Second)}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(asked()) != 0 {
		t.Fatalf("proxy asked for %v", asked())
	}
}
