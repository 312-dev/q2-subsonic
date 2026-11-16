package tlsroots

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// A server on the owner's tailnet is reached through the Q2's tailscaled (Q2 Pod's System Settings >
// Tailscale). The Q2's kernel has no TUN driver, so tailscaled runs in userspace mode and the tailnet
// is only reachable through its local SOCKS5 proxy, which Q2 Pod names in tailnetState while it is
// connected. Only tailnet destinations use it, so the rest of the internet goes direct: a host whose
// name ends in .ts.net (MagicDNS), a Tailscale address (100.64.0.0/10, fd7a:115c:a1e0::/48), or a
// public name that resolves to one (a domain pointed at a tailnet IP). Certificates are checked
// exactly as without it: the proxy only carries the TLS connection.
var (
	tailnetState = "/tmp/q2-tailscale.state" // injectable for tests

	tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

	lookupHost = func(ctx context.Context, host string) ([]netip.Addr, error) { // injectable for tests
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}

	tnMu    sync.Mutex
	tnRead  time.Time             // tailnetState last read
	tnProxy *url.URL              // its proxy, nil when tailscaled is not connected
	tnHosts = map[string]hostAt{} // a name's answer, so a stream's every range does not look it up
	tnNow   = time.Now            // injectable for tests
)

type hostAt struct {
	tailnet bool
	at      time.Time
}

const (
	stateEvery  = 2 * time.Second // tailnetState read again at most this often
	hostEvery   = 5 * time.Minute // a name's answer kept this long
	lookupLimit = 3 * time.Second
)

// IsTailnetAddr reports whether a is a Tailscale address.
func IsTailnetAddr(a netip.Addr) bool {
	a = a.Unmap()
	return tailnetV4.Contains(a) || tailnetV6.Contains(a)
}

// tailnetProxy is tailscaled's SOCKS5 proxy from tailnetState, read again every stateEvery; nil
// while it is not connected.
func tailnetProxy() *url.URL {
	tnMu.Lock()
	defer tnMu.Unlock()
	now := tnNow()
	if !tnRead.IsZero() && now.Sub(tnRead) < stateEvery && !now.Before(tnRead) {
		return tnProxy
	}
	tnRead, tnProxy = now, nil
	f, err := os.Open(tailnetState)
	if err != nil {
		return nil
	}
	defer f.Close()
	running := false
	var proxy string
	for s := bufio.NewScanner(f); s.Scan(); {
		k, v, _ := strings.Cut(s.Text(), "=")
		switch k {
		case "state":
			running = v == "running"
		case "proxy":
			proxy = v
		}
	}
	if u, err := url.Parse(proxy); running && err == nil && u.Scheme == "socks5" && u.Host != "" {
		tnProxy = u
	}
	return tnProxy
}

// onTailnet reports whether host is a tailnet destination: a Tailscale address, a MagicDNS name, or
// a name that resolves to a Tailscale address (looked up through the normal resolver, which works
// without the tailnet, its answer kept for hostEvery).
func onTailnet(ctx context.Context, host string) bool {
	if a, err := netip.ParseAddr(host); err == nil {
		return IsTailnetAddr(a)
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if strings.HasSuffix(h, ".ts.net") {
		return true
	}
	tnMu.Lock()
	c, ok := tnHosts[h]
	tnMu.Unlock()
	now := tnNow()
	if ok && now.Sub(c.at) < hostEvery && !now.Before(c.at) {
		return c.tailnet
	}
	ctx, cancel := context.WithTimeout(ctx, lookupLimit)
	defer cancel()
	addrs, err := lookupHost(ctx, h)
	if err != nil {
		return false // unknown here: direct, and the dial reports the failure
	}
	tailnet := false
	for _, a := range addrs {
		tailnet = tailnet || IsTailnetAddr(a)
	}
	tnMu.Lock()
	tnHosts[h] = hostAt{tailnet, now}
	tnMu.Unlock()
	return tailnet
}

// TailnetProxy is http.Transport's Proxy: tailscaled's for a tailnet destination while it is
// connected, else none (direct).
func TailnetProxy(req *http.Request) (*url.URL, error) {
	p := tailnetProxy()
	if p == nil || req.URL == nil || !onTailnet(req.Context(), req.URL.Hostname()) {
		return nil, nil
	}
	return p, nil
}
