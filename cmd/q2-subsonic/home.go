package main

import (
	"net"
	"net/url"
	"strings"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// The home address is the server's address on the home network (settings.json's home), which the
// client asks first while the Q2 is on that network: a server at home that its public https://
// address can't reach from inside (no hairpin NAT), or that is quicker asked directly. It is a
// private IPv4 address (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16), so it may be plain http://, and
// "on that network" is the Q2's own subnet holding it, from its interfaces' addresses and masks.

// homeHint is the words for a home address that isn't one, for the phone.
const homeHint = "Use the server's address on your home network, such as http://192.168.1.20:4533. It must be a 10.x, 172.16 to 172.31, or 192.168.x address."

// cleanHome is the home address as the phone typed it, tidied as cleanServer tidies the server's,
// with http:// added when no scheme is typed; "" is none. Its host must be a private IPv4 address.
func cleanHome(raw string) (string, *signinRefused) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || !subsonic.Private(u.Hostname()) {
		return "", &signinRefused{"home", homeHint}
	}
	return tidy(u), nil
}

// localAddrs is the addresses, with their masks, of the Q2's interfaces that are up, the loopback
// left out.
func localAddrs() []net.Addr {
	ifs, _ := net.Interfaces()
	var out []net.Addr
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		a, _ := i.Addrs()
		out = append(out, a...)
	}
	return out
}

// inNets is whether ip is in the subnet of one of addrs.
func inNets(ip net.IP, addrs []net.Addr) bool {
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.Contains(ip) {
			return true
		}
	}
	return false
}

// nearHome is whether the Q2 is on home's network now, asked of addrs (localAddrs when nil) at each
// call; always false for an address without an IP.
func nearHome(home string, addrs func() []net.Addr) func() bool {
	if addrs == nil {
		addrs = localAddrs
	}
	var ip net.IP
	if u, err := url.Parse(home); err == nil {
		ip = net.ParseIP(u.Hostname())
	}
	return func() bool { return ip != nil && inNets(ip, addrs()) }
}
