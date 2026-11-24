package tlsroots

import (
	"cmp"
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// A server's name is looked up for every new connection, and the network the Q2 joins may answer
// that badly while the server itself can be reached: a venue's resolver that fails the name
// ("server misbehaving") or drops the query (5 s per try, two tries), and, just after a lease,
// Go's resolver still holding the resolver file from before it (read again at most every 5 s), with
// no server in it. So the dialler remembers, per name, the address it last connected to, and
// dials that address when the name doesn't resolve, or hasn't resolved after slowLookup. TLS still
// checks the certificate against the name (the transport handshakes with the request's host), so
// an old address can only reach the server it reached before.

// slowLookup is how long a lookup may take before a remembered address is dialled instead: far
// above the 23 to 380 ms the Q2's log shows for an answer, far below the resolver's 5 s per try.
var slowLookup = time.Second

// lookupHost resolves a name (injectable for tests).
var lookupHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// logf, when set (SetLog), is told each time a remembered address stands in for a name.
var logf atomic.Pointer[func(format string, a ...any)]

// SetLog sets where the dialler says that a remembered address stood in for a name.
func SetLog(f func(format string, a ...any)) { logf.Store(&f) }

var (
	lastMu   sync.Mutex
	lastAddr = map[string]netip.Addr{} // a name: the address last connected to for it
)

// remembered is the address host last connected to; invalid when none.
func remembered(host string) netip.Addr {
	lastMu.Lock()
	defer lastMu.Unlock()
	return lastAddr[host]
}

func remember(host string, c net.Conn) {
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		lastMu.Lock()
		lastAddr[host] = ap.Addr().Unmap()
		lastMu.Unlock()
	}
}

// forget drops every remembered address (tests).
func forget() {
	lastMu.Lock()
	clear(lastAddr)
	lastMu.Unlock()
}

// dialer is the transport's DialContext: d dials, with the name looked up here so a remembered
// address can stand in for it.
type dialer struct{ d net.Dialer }

func (dl *dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return dl.d.DialContext(ctx, network, addr)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return dl.d.DialContext(ctx, network, addr)
	}
	type answer struct {
		ips []netip.Addr
		err error
	}
	got := make(chan answer, 1)
	go func() {
		ips, err := lookupHost(ctx, host)
		got <- answer{ips, err}
	}()
	known := remembered(host)
	var a answer
	if !known.IsValid() {
		a = <-got
	} else {
		t := time.NewTimer(slowLookup)
		select {
		case a = <-got:
		case <-t.C:
		}
		t.Stop()
	}
	if known.IsValid() && (a.err != nil || a.ips == nil) { // failed, or no answer yet
		why := "no answer in " + slowLookup.String()
		if a.err != nil {
			why = a.err.Error()
		}
		if f := logf.Load(); f != nil {
			(*f)("dial: the server's name: %s; dialling its last address", why)
		}
		c, err := dl.d.DialContext(ctx, network, net.JoinHostPort(known.String(), port))
		if err == nil {
			remember(host, c)
			return c, nil
		}
		if a.err != nil || ctx.Err() != nil {
			return nil, err
		}
		a = <-got // the old address failed, the lookup still runs: its answer, when it has one
	}
	if a.err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: a.err}
	}
	c, err := dl.dialEach(ctx, network, a.ips, port)
	if err == nil {
		remember(host, c)
	}
	return c, err
}

// dialEach dials ips in turn, IPv4 first (the Q2 has no IPv6), until one connects.
func (dl *dialer) dialEach(ctx context.Context, network string, ips []netip.Addr, port string) (net.Conn, error) {
	ips = slices.Clone(ips)
	slices.SortStableFunc(ips, func(a, b netip.Addr) int {
		return cmp.Compare(boolInt(!a.Unmap().Is4()), boolInt(!b.Unmap().Is4()))
	})
	err := errors.New("dial: the name has no address")
	for _, ip := range ips {
		var c net.Conn
		if c, err = dl.d.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port)); err == nil {
			return c, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
