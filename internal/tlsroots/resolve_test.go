package tlsroots

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDNS is a name server on addr:port that answers every A question with 127.0.0.1 and every
// other with no records; silent, it reads the questions and answers none.
type fakeDNS struct {
	conn   *net.UDPConn
	asked  atomic.Int32
	silent bool
}

func newFakeDNS(t *testing.T, ip string, port int, silent bool) *fakeDNS {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(ip), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeDNS{conn: c, silent: silent}
	t.Cleanup(func() { c.Close() })
	go s.serve()
	return s
}

func (s *fakeDNS) port() int { return s.conn.LocalAddr().(*net.UDPAddr).Port }

func (s *fakeDNS) serve() {
	buf := make([]byte, 512)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		s.asked.Add(1)
		if s.silent || n < 12 {
			continue
		}
		q := buf[:n]
		end := 12 // the question: its name's labels, then type and class
		for end < n && q[end] != 0 {
			end += int(q[end]) + 1
		}
		end += 5
		if end > n {
			continue
		}
		qtype := binary.BigEndian.Uint16(q[end-4:])
		out := append([]byte{}, q[:end]...)
		binary.BigEndian.PutUint16(out[2:], 0x8180) // a response, recursion available, no error
		binary.BigEndian.PutUint16(out[6:], 0)      // answers
		binary.BigEndian.PutUint16(out[8:], 0)
		binary.BigEndian.PutUint16(out[10:], 0)
		if qtype == 1 {
			binary.BigEndian.PutUint16(out[6:], 1)
			out = append(out, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 127, 0, 0, 1)
		}
		s.conn.WriteToUDP(out, from)
	}
}

// resolverFile points the lookups at a resolver file in a test folder, its servers on port, and
// returns the file's path (not created).
func resolverFile(t *testing.T, port int) string {
	oldFile, oldPort, oldTry := resolvFile, dnsPort, dnsTry
	t.Cleanup(func() { resolvFile, dnsPort, dnsTry = oldFile, oldPort, oldTry })
	resolvFile = filepath.Join(t.TempDir(), "resolv.conf")
	dnsPort = strconv.Itoa(port)
	dnsTry = 400 * time.Millisecond
	return resolvFile
}

// lease writes the file as udhcpc's script does at a lease: truncated in place, then a line per
// server appended, all under the truncation's mtime (the Q2's kernel stamps files with the coarse
// clock, so writes in one tick share it).
func lease(t *testing.T, path string, servers ...string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, _ := os.Stat(path)
	f, _ = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	for _, s := range servers {
		f.WriteString("nameserver " + s + " # wlan0\n")
	}
	f.Close()
	os.Chtimes(path, fi.ModTime(), fi.ModTime())
}

func lookupOK(t *testing.T, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := lookupHost(ctx, "music.example")
	if err != nil || len(ips) == 0 || ips[0] != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("%s: %v %v", what, ips, err)
	}
}

// The boot of the stuck resolver: no resolver file at the app's first lookups (no lease yet),
// then the lease's, written in place under one mtime: the next lookup uses its server, and the
// one after a second network's lease uses the new server.
func TestALeaseIsPickedUpAtTheNextLookup(t *testing.T) {
	home := newFakeDNS(t, "127.0.0.2", 0, false)
	away := newFakeDNS(t, "127.0.0.3", home.port(), false)
	path := resolverFile(t, home.port())
	ctx := context.Background()
	_, err := lookupHost(ctx, "music.example")
	var de *net.DNSError
	if !errors.As(err, &de) || de.Server != path || de.Err != ErrNoNameServer.Error() {
		t.Fatalf("no file: %v; want the file named as listing no server", err)
	}
	lease(t, path) // touched, nothing in it yet
	if _, err := lookupHost(ctx, "music.example"); err == nil {
		t.Fatal("an empty file answered")
	}
	lease(t, path, "127.0.0.2")
	lookupOK(t, "the home lease")
	forget()
	t.Cleanup(forget)
	c, _, url := tlsServer(t) // a request by the name, through the transport's dialler
	if err := get(c, url); err != nil {
		t.Fatalf("a request after the lease: %v", err)
	}
	lease(t, path, "127.0.0.3") // another network
	before := home.asked.Load()
	lookupOK(t, "the second network's lease")
	if home.asked.Load() != before || away.asked.Load() == 0 {
		t.Fatalf("asked home %d more times, away %d; want the second network's server only", home.asked.Load()-before, away.asked.Load())
	}
}

// A server that reads the question and never answers is given dnsTry, then the next is asked.
func TestASilentNameServerGivesWayToTheNext(t *testing.T) {
	dead := newFakeDNS(t, "127.0.0.2", 0, true)
	newFakeDNS(t, "127.0.0.3", dead.port(), false)
	path := resolverFile(t, dead.port())
	lease(t, path, "127.0.0.2", "127.0.0.3")
	start := time.Now()
	lookupOK(t, "the first server silent")
	if took := time.Since(start); took > dnsTry+time.Second {
		t.Fatalf("took %v; want about %v", took, dnsTry)
	}
	if dead.asked.Load() == 0 {
		t.Fatal("the first server was never asked")
	}
}

// With its only server silent, a lookup fails in dnsTry, naming that server; a name reached
// before is then dialled at its last address (TestARememberedAddressStandsInForAFailingOrSlowName).
func TestAnOnlySilentNameServerFailsInItsTime(t *testing.T) {
	dead := newFakeDNS(t, "127.0.0.2", 0, true)
	path := resolverFile(t, dead.port())
	lease(t, path, "127.0.0.2")
	start := time.Now()
	_, err := lookupHost(context.Background(), "music.example")
	var de *net.DNSError
	if !errors.As(err, &de) || de.Server != "127.0.0.2:"+dnsPort {
		t.Fatalf("got %v; want a DNS error from 127.0.0.2", err)
	}
	if took := time.Since(start); took > dnsTry+time.Second {
		t.Fatalf("took %v; want about %v", took, dnsTry)
	}
}

// A server's comment and a search line are not servers.
func TestNameServersReadsTheFileAsWritten(t *testing.T) {
	path := resolverFile(t, 53)
	os.WriteFile(path, []byte("search lan # wlan0\nnameserver 10.16.0.1 # wlan0\n# nameserver 9.9.9.9\nnameserver 1.0.0.1\n"), 0o644)
	got := NameServers()
	if len(got) != 2 || got[0] != "10.16.0.1" || got[1] != "1.0.0.1" {
		t.Fatalf("%q", got)
	}
}
