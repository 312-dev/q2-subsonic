// Package tlsroots gives HTTPS clients an embedded Mozilla CA bundle (the Q2 has none) and a
// trustworthy clock: the Q2's is often unset (1970/2000), which fails certificate checks, so when
// the system year is before 2025 the time comes from a server's HTTP Date header instead.
package tlsroots

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

//go:embed cacert.pem
var bundle []byte

const retryAfter = 30 * time.Second

// An HTTP/2 connection's health check (newTransport): a ping after h2PingAfter with nothing read,
// the connection closed after h2PingWait with no answer, so a dead one is left within 15 s. A
// download reads all the time and is never pinged; a quiet one is pinged about every 10 s (an
// answer is a read), a few bytes each.
const (
	h2PingAfter = 10 * time.Second
	h2PingWait  = 5 * time.Second
)

var (
	once sync.Once
	pool *x509.CertPool

	mu       sync.Mutex
	dateURL  string
	offset   time.Duration // server time minus system time, once learned
	have     bool
	fetching chan struct{} // closed when the running Date fetch ends; nil when none runs
	lastTry  time.Time
	nowFn    = time.Now // injectable for tests

	dateRoots = Pool // injectable for tests

	transports = map[time.Duration]*http.Transport{}
)

// Pool is the embedded Mozilla roots.
func Pool() *x509.CertPool {
	once.Do(func() {
		pool = x509.NewCertPool()
		pool.AppendCertsFromPEM(bundle)
	})
	return pool
}

// SetDateURL sets the URL whose Date header supplies the time when the system clock is unset.
func SetDateURL(u string) {
	mu.Lock()
	dateURL, have, lastTry, offset = u, false, time.Time{}, 0
	mu.Unlock()
}

// DateURL is the URL set by SetDateURL.
func DateURL() string { mu.Lock(); defer mu.Unlock(); return dateURL }

// Now is the system time when its year is 2025 or later, else the server's time (cached as an offset
// from the system clock). One fetch runs at a time (at most 5 s), and other callers wait for it. For
// 30 s after a failed fetch it is the system time, never a wait.
func Now() time.Time {
	sys := nowFn()
	if sys.Year() >= 2025 {
		return sys
	}
	mu.Lock()
	if wait := fetching; wait != nil && !have {
		mu.Unlock()
		select {
		case <-wait:
		case <-time.After(6 * time.Second):
		}
		mu.Lock()
		off := offset
		mu.Unlock()
		return nowFn().Add(off)
	}
	if have || dateURL == "" || (!lastTry.IsZero() && sys.Sub(lastTry) < retryAfter) {
		off := offset
		mu.Unlock()
		return sys.Add(off)
	}
	done := make(chan struct{})
	fetching, lastTry = done, sys
	u := dateURL
	mu.Unlock()

	d, err := fetchDate(u)
	mu.Lock()
	fetching = nil
	if err == nil {
		offset, have = d.Sub(sys), true
	}
	off := offset
	mu.Unlock()
	close(done)
	return sys.Add(off)
}

// Known reports whether Now is trustworthy: the system clock is set, or a server's Date was read.
func Known() bool {
	if nowFn().Year() >= 2025 {
		return true
	}
	mu.Lock()
	defer mu.Unlock()
	return have
}

// fetchDate reads u's Date header. The handshake is fully verified (chain to the roots, and u's host
// name or IP address); only the validity period is judged at the leaf's own NotBefore, as the time
// is what is being asked. A redirect is not followed: the 3xx carries a Date too, and following it
// could read the Date from plain http.
func fetchDate(u string) (time.Time, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return time.Time{}, err
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: dateTransport(dateRoots(), pu.Hostname()),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Head(u)
	if err != nil {
		return time.Time{}, err
	}
	resp.Body.Close()
	return http.ParseTime(resp.Header.Get("Date"))
}

// dateTransport verifies the server against roots and host. The handshake's ServerName can't be
// used: it is empty for an IP address, which would skip the name check.
func dateTransport(roots *x509.CertPool, host string) *http.Transport {
	return &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Go can only run its own checks at a fixed Time, so verify here instead; this is the
			// documented pattern for custom verification, and it checks everything but the date.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("tlsroots: no certificate")
				}
				leaf := cs.PeerCertificates[0]
				inter := x509.NewCertPool()
				for _, c := range cs.PeerCertificates[1:] {
					inter.AddCert(c)
				}
				_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter,
					DNSName: host, CurrentTime: leaf.NotBefore.Add(time.Second)})
				return err
			},
		},
	}
}

// Client is an HTTPS client verifying against Pool() at Now(). The timeout bounds connecting, the
// handshake and the wait for headers, not the body, so long downloads and streams are not cut.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: transport(Pool(), timeout)}
}

func transport(roots *x509.CertPool, timeout time.Duration) *http.Transport {
	mu.Lock()
	defer mu.Unlock()
	return newTransport(roots, timeout)
}

func newTransport(roots *x509.CertPool, timeout time.Duration) *http.Transport {
	if t := transports[timeout]; t != nil && roots == pool {
		return t
	}
	// HTTP/2 when the server offers it (a song's download, its playing-now report, a cover and the
	// ping share one connection, and a download cancelled at a skip resets its stream, not the
	// connection), and TLS sessions resumed: each full handshake is a chain check in pure Go on
	// the Q2's CPU. An idle connection goes after 90 s rather than being held. An HTTP/2
	// connection that has read nothing for h2PingAfter is pinged, and closed when no answer comes
	// in h2PingWait: one that dies without a reset (the Wi-Fi joining another network takes its
	// address) is otherwise kept, as a request timed out on it resets only its stream, and every
	// request goes to it until the kernel gives the connection up, about 15 minutes on. A name
	// that fails to resolve, or resolves slowly, is dialled at the address it last had (dialer).
	t := &http.Transport{
		DialContext:           (&dialer{d: net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
		TLSClientConfig: &tls.Config{RootCAs: roots, Time: Now, MinVersion: tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(16)},
		HTTP2: &http.HTTP2Config{SendPingTimeout: h2PingAfter, PingTimeout: h2PingWait},
	}
	if roots == pool {
		transports[timeout] = t
	}
	return t
}
