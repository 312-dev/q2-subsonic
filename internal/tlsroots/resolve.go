package tlsroots

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
)

// The name servers are read from the resolver file at every lookup, and each is asked through a
// net.Resolver of its own, so no lookup depends on Go's copy of the file. Go reads /etc/resolv.conf
// again only when its mtime differs from the one it holds. On the Q2, /etc/resolv.conf is a link
// into /tmp that udhcpc's script rewrites in place at each lease: it truncates the file, then
// appends a line per server, and the Q2's kernel stamps files with the coarse clock, so the
// appends can carry the truncation's mtime. Go's copy read in between (by the app's own check,
// woken by that very write) holds "no server" under the final mtime, and every lookup after it goes
// to Go's default, [::1]:53, until the file's mtime moves again.

// resolvFile is the resolver file (injectable for tests).
var resolvFile = "/etc/resolv.conf"

// dnsPort is the name servers' port (injectable for tests: a test's server can't listen on 53).
var dnsPort = "53"

// dnsTry is how long one name server is given before the next is asked.
var dnsTry = 2 * time.Second

// ErrNoNameServer is a lookup made while the resolver file lists no name server: the Wi-Fi has no
// lease yet, or the file is being rewritten.
var ErrNoNameServer = errors.New("the resolver file lists no name server")

// NameServers are the name servers the resolver file lists now, in its order.
func NameServers() []string {
	b, err := os.ReadFile(resolvFile)
	if err != nil {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if a, err := netip.ParseAddr(f[1]); err == nil {
				out = append(out, a.String())
			}
		}
	}
	return out
}

// LookupIP resolves host with the name servers the resolver file lists now, each in turn for
// dnsTry. A name the server says doesn't exist is not asked of the next. The error is a
// *net.DNSError naming the server that gave it, or naming the file when it lists none.
func LookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	servers := NameServers()
	if len(servers) == 0 {
		return nil, &net.DNSError{Err: ErrNoNameServer.Error(), Name: host, Server: resolvFile, IsTemporary: true}
	}
	var last error
	for _, s := range servers {
		at := net.JoinHostPort(s, dnsPort)
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, at) // whatever server Go's own copy of the file names
		}}
		tctx, cancel := context.WithTimeout(ctx, dnsTry)
		ips, err := r.LookupNetIP(tctx, "ip", host)
		cancel()
		if err == nil {
			return ips, nil
		}
		var de *net.DNSError
		if errors.As(err, &de) {
			named := *de
			named.Server = at
			err = &named
			if de.IsNotFound {
				return nil, err
			}
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}
