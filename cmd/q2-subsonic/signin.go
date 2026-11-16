package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/creds"
	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/qr"
	"github.com/andsuor/q2-subsonic/internal/subsonic"
	"github.com/andsuor/q2-subsonic/internal/tlsroots"
)

//go:embed signin.html
var signinHTML []byte

const (
	signinCookie   = "q2sub"
	signinTimeout  = 10 * time.Minute
	pinTries       = 10
	pinLock        = time.Minute
	signinTimedOut = "Sign-in timed out"
	signinConns    = 16 // open connections the sign-in page takes at once
)

// listenTCP listens on port, or on the next nine when it is taken; port 0 takes any free one.
func listenTCP(port int) (net.Listener, int) {
	n := 10
	if port == 0 {
		n = 1
	}
	for p := port; p < port+n; p++ {
		if l, err := net.Listen("tcp4", fmt.Sprintf(":%d", p)); err == nil {
			return l, l.Addr().(*net.TCPAddr).Port
		}
	}
	return nil, 0
}

// signin is the phone page for entering the Navidrome sign-in. It listens only between start and
// stop (a success, the Q2 leaving the QR page, or ten minutes), behind a one-time key (in the QR
// code) or the PIN on the Q2's screen, kept in an HttpOnly SameSite=Strict cookie. POSTs need X-Q2: 1.
//
// The Q2's QR page follows the phone through the state file's step, title and line (step: phone
// once the page is open past the key or PIN, check while the server is asked, done, fail (not
// reached) or error after),
// which Q2 Pod with the "signin" word draws as a page of its own in place of the code
// (addon.c qr_refresh); note says the same for one without it.
type signin struct {
	dir       string
	state     interface{ Write(map[string]string) error }              // the app's state file (q2.State, or the daemon's merge)
	now       func() time.Time                                         // default time.Now
	tick      time.Duration                                            // how often the 10 min limit is checked; default 1 s
	newClient func(subsonic.Creds) (*subsonic.Client, error)           // default: the embedded roots
	lookup    func(ctx context.Context, host string) ([]net.IP, error) // default net.DefaultResolver's

	mu       sync.Mutex
	ln       net.Listener
	srv      *http.Server
	done     chan struct{}
	key, pin string
	deadline time.Time
	fails    int
	lockout  time.Time
	ok       bool
	msg      string
	failed   string            // the last phone sign-in's failure, still shown after a restart until the next try
	conns    chan struct{}     // one slot per open connection (capListener)
	shown    map[string]string // the QR page's keys as start wrote them
	step     map[string]string // the step keys last written over them (report), nil before the phone came
}

const (
	stepPhone = "phone" // the phone has the form: "Continue on Your Phone"
	stepCheck = "check" // the server is being asked
	stepDone  = "done"  // signed in; Q2 Pod goes back to the app's page, now signed in
	stepFail  = "fail"  // the last try couldn't reach the server
	stepError = "error" // the last try failed otherwise (refused, not a Subsonic server…), or the time ran out
)

// lastServerFile keeps the address of the last server signed in to, for the next sign-in's form.
const lastServerFile = "last-server"

func (s *signin) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *signin) active() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.ln != nil }

// result is how the last sign-in ended: whether it worked, and the words for the Q2's screen.
func (s *signin) result() (bool, string) { s.mu.Lock(); defer s.mu.Unlock(); return s.ok, s.msg }

// start opens the page and writes the QR code and PIN to the state file. It does nothing while open.
func (s *signin) start(port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return
	}
	ln, p := listenTCP(port)
	if ln == nil {
		s.ok, s.msg = false, "Couldn't open the sign-in page"
		return
	}
	s.ln, s.done = ln, make(chan struct{})
	s.key, s.pin = q2.Key(16), q2.PIN(6)
	s.deadline = s.clock().Add(signinTimeout)
	s.fails, s.lockout, s.ok, s.msg, s.step = 0, time.Time{}, false, s.failed, nil
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/login", s.login)
	mux.HandleFunc("/api/signin", s.signin)
	// Any LAN host can reach it: bound the headers, the time to send a request and the open
	// connections, so none can pin the Q2's memory or file descriptors.
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	s.conns = make(chan struct{}, signinConns)
	go s.srv.Serve(capListener{ln, s.conns})
	go s.watch(s.done)

	kv := map[string]string{"pin": s.pin, "note": "Scan with your phone to sign in"} // under the code
	if ip := q2.LocalIP(); ip == "" {
		kv["addr"], kv["qr"], kv["qrsize"], kv["note"] = "", "", "0", "Connect the Q2 to Wi-Fi"
	} else {
		kv["addr"] = fmt.Sprintf("%s:%d", ip, p)
		kv["qr"], kv["qrsize"] = "", "0"
		if c, err := qr.Encode([]byte(fmt.Sprintf("http://%s/?k=%s", kv["addr"], s.key))); err == nil {
			kv["qr"], kv["qrsize"] = c.Hex(), fmt.Sprint(c.Size)
		}
	}
	s.shown = kv
	s.state.Write(kv)
}

// report shows step on the Q2's QR page, with its title and line, the code and PIN kept under them
// with the note, which a firmware without steps draws beside the code. Under s.mu; the caller
// writes the map it returns.
func (s *signin) report(step, title, line, note string) map[string]string {
	kv := maps.Clone(s.shown)
	s.step = map[string]string{"step": step, "title": title, "line": line, "note": note}
	maps.Copy(kv, s.step)
	return kv
}

// opened is the phone past the key or the PIN: the Q2 says to go on there. A page loaded again
// leaves a later step (checking, a failure) as it is.
func (s *signin) opened() {
	s.mu.Lock()
	var kv map[string]string
	if s.ln != nil && s.step == nil {
		kv = s.report(stepPhone, "Continue on Your Phone", "Enter your server and password there", s.shown["note"])
	}
	s.mu.Unlock()
	if kv != nil {
		s.state.Write(kv)
	}
}

// watch ends the sign-in when its ten minutes are up.
func (s *signin) watch(done chan struct{}) {
	d := s.tick
	if d == 0 {
		d = time.Second
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.mu.Lock()
			late := !s.clock().Before(s.deadline)
			if late {
				s.ok, s.msg, s.failed = false, signinTimedOut, ""
			}
			s.mu.Unlock()
			if late {
				s.stop()
				return
			}
		}
	}
}

// stop closes the page and clears the keys it wrote from the state file.
func (s *signin) stop() {
	s.mu.Lock()
	if s.ln == nil {
		s.mu.Unlock()
		return
	}
	srv := s.srv
	s.ln, s.srv = nil, nil
	close(s.done)
	// After a success or the timeout the Q2 may still show the QR page: it says what happened (the
	// step's page, or beside the code the note, which a firmware without steps draws only there:
	// without addr it says "Connect the Q2 to Wi-Fi"). The PIN goes; the server behind it is closed.
	kv := map[string]string{}
	if s.ok || s.msg == signinTimedOut {
		kv = map[string]string{"addr": s.shown["addr"], "qr": s.shown["qr"], "qrsize": s.shown["qrsize"]}
		if s.ok {
			maps.Copy(kv, s.step) // done, written by the sign-in
		} else {
			kv["step"], kv["title"], kv["line"] = stepError, "Sign-In Timed Out", "Press Return, then sign in again"
		}
		kv["note"] = s.msg + " · Press Return"
	}
	s.mu.Unlock()
	s.state.Write(kv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if srv.Shutdown(ctx) != nil {
		srv.Close()
	}
}

// guess checks got against secret, counting a miss towards the lockout (pinTries wrong ones in a
// row lock every guess for a minute).
func (s *signin) guess(secret, got string) (ok, locked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if now.Before(s.lockout) {
		return false, true
	}
	if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1 {
		s.fails = 0
		return true, false
	}
	if s.fails++; s.fails >= pinTries {
		s.fails, s.lockout = 0, now.Add(pinLock)
	}
	return false, false
}

func (s *signin) authed(r *http.Request) bool {
	c, err := r.Cookie(signinCookie)
	s.mu.Lock()
	defer s.mu.Unlock()
	return err == nil && s.key != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.key)) == 1
}

func (s *signin) setCookie(w http.ResponseWriter) {
	s.mu.Lock()
	v := s.key
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: signinCookie, Value: v, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]string{"error": msg})
}

// index serves the page. The QR code's link carries the key (?k=): the form is served at once with
// the cookie, not after a redirect, since a phone opening the link from its camera is a cross-site
// navigation, and a SameSite=Strict cookie set on a redirect is not sent on the request it leads to,
// which would land on the PIN step. Without the key or the cookie the page asks for the PIN first;
// the form is not in it until the PIN is taken (the script shows it then).
func (s *signin) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	code, page, authed := http.StatusOK, signinHTML, s.authed(r)
	if k := r.URL.Query().Get("k"); k != "" && !authed {
		s.mu.Lock()
		key := s.key
		s.mu.Unlock()
		if ok, locked := s.guess(key, k); ok {
			s.setCookie(w)
			authed = true
		} else if locked {
			code = http.StatusTooManyRequests
		}
	}
	if authed {
		s.opened()
		if last := s.lastServer(); last != "" {
			page = bytes.Replace(page, []byte(`data-server=""`), []byte(`data-server="`+html.EscapeString(last)+`"`), 1)
		}
	} else {
		code, page = max(code, http.StatusUnauthorized), bytes.Replace(signinHTML, []byte(`data-auth="1"`), []byte(`data-auth="0"`), 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(code)
	w.Write(page)
}

// lastServer is the address of the last server signed in to, for the form; "" when none.
func (s *signin) lastServer() string {
	b, err := os.ReadFile(filepath.Join(s.dir, lastServerFile))
	if err != nil || len(b) > 512 {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// login takes the PIN from the Q2's screen.
func (s *signin) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Q2") != "1" {
		refuse(w, http.StatusForbidden, "bad request")
		return
	}
	var body struct{ PIN string }
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body)
	s.mu.Lock()
	pin := s.pin
	s.mu.Unlock()
	ok, locked := s.guess(pin, strings.TrimSpace(body.PIN))
	switch {
	case locked:
		refuse(w, http.StatusTooManyRequests, "Too many tries. Wait a minute.")
	case !ok:
		refuse(w, http.StatusForbidden, "That isn't the PIN on the Q2's screen.")
	default:
		s.setCookie(w)
		s.opened()
		reply(w, http.StatusOK, map[string]any{"ok": true, "server": s.lastServer()}) // the form's, as index fills it
	}
}

// signinRefused is a sign-in the phone can't send: field names the input it concerns ("pin",
// "server", or "" for the user name and password), where the page shows it.
type signinRefused struct{ field, msg string }

// cleanServer is the server address as the phone typed or pasted it, tidied: https:// added when it
// has no scheme, an API path copied with it (/rest/ping.view?…) cut off, and the trailing slash.
// http:// is refused: the password would cross the network unencrypted, and subsonic.New refuses it
// anyway. The words never echo the address, which may hold a user:password@.
func cleanServer(raw string) (string, *signinRefused) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", &signinRefused{"server", "Enter your server's address."}
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", &signinRefused{"server", "That isn't a server address. It looks like https://music.example.com"}
	}
	if u.Scheme == "http" {
		return "", &signinRefused{"server", "The Q2 signs in only over https://, so your password never crosses the network unencrypted. Use your server's https:// address."}
	}
	p := u.Path
	if i := strings.Index(strings.ToLower(p+"/"), "/rest/"); i >= 0 {
		p = p[:i]
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment, u.ForceQuery = strings.TrimRight(p, "/"), "", "", "", false
	return u.String(), nil
}

// tailnet is whether host is a Tailscale address: a MagicDNS name (*.ts.net) or one in Tailscale's
// 100.64.0.0/10, as typed or as it resolves.
func (s *signin) tailnet(ctx context.Context, host string) bool {
	if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".ts.net") {
		return true
	}
	cgnat := &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	if ip := net.ParseIP(host); ip != nil {
		return cgnat.Contains(ip)
	}
	look := s.lookup
	if look == nil {
		look = func(ctx context.Context, h string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", h)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, _ := look(ctx, host)
	for _, ip := range ips {
		if cgnat.Contains(ip) {
			return true
		}
	}
	return false
}

// Words for a server the Q2 can't reach: the phone's, and the Q2's page's line. A tailnet address is
// out of the Q2's reach for good (it can't join a tailnet), so the words point to the server's public
// address instead.
const (
	reachTS      = "This server is only on Tailscale; use its public address instead."
	reachTSLine  = "Only on Tailscale: use its public address"
	reachLAN     = "Can't reach this server from the Q2's Wi-Fi."
	reachLANLine = "Can't reach this server from the Q2's Wi-Fi"
)

// failure is a sign-in's error as the phone shows it (the field it concerns and its words) and as
// the Q2's page does (step, title and line); subsonic.Message's short words are the root row's.
func (s *signin) failure(ctx context.Context, err error, host, hostname string) (field, phone, step, title, line string) {
	msg := subsonic.Message(err, host)
	var ae *subsonic.APIError
	answered := errors.As(err, &ae) && ae.HTTP != 0 // "The server answered HTTP 404": it was reached
	switch {
	case errors.Is(err, subsonic.ErrUnreachable) && !answered && !errors.Is(err, subsonic.ErrStalled):
		if s.tailnet(ctx, hostname) {
			return "server", reachTS, stepFail, "Can't Reach the Server", reachTSLine
		}
		return "server", reachLAN, stepFail, "Can't Reach the Server", reachLANLine
	case errors.Is(err, subsonic.ErrAuth), errors.Is(err, subsonic.ErrAuthKey):
		return "", msg, stepError, "Sign-In Refused", msg
	case errors.Is(err, subsonic.ErrCert), errors.Is(err, subsonic.ErrNotSubsonic), errors.Is(err, subsonic.ErrInsecure),
		errors.Is(err, subsonic.ErrUnreachable):
		return "server", msg, stepError, "Couldn't Sign In", msg
	}
	return "", msg, stepError, "Couldn't Sign In", msg
}

func (s *signin) signin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Q2") != "1" {
		refuse(w, http.StatusForbidden, "bad request")
		return
	}
	if !s.authed(r) {
		msg := "Enter the PIN shown on the Q2 first."
		if _, err := r.Cookie(signinCookie); err == nil { // a key from an earlier sign-in
			msg = "Sign-in was restarted on the Q2. Open Sign In from Your Phone again and scan the new code."
		}
		reply(w, http.StatusUnauthorized, map[string]string{"error": msg, "field": "pin"})
		return
	}
	var body struct{ Server, User, Password string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		refuse(w, http.StatusBadRequest, "bad body")
		return
	}
	answer := func(ok bool, field, msg string) {
		reply(w, http.StatusOK, map[string]any{"ok": ok, "message": msg, "field": field})
	}
	server, bad := cleanServer(body.Server)
	if bad != nil {
		answer(false, bad.field, bad.msg)
		return
	}
	c := subsonic.Creds{Server: server, User: strings.TrimSpace(body.User), Password: body.Password}
	u, _ := url.Parse(server) // cleanServer's: it parses, with a host
	host, hostname := u.Host, u.Hostname()
	s.mu.Lock()
	kv := s.report(stepCheck, "Signing In…", "Checking with "+hostname, "Checking with "+hostname)
	s.mu.Unlock()
	s.state.Write(kv)
	// fin ends a try: msg, short, is the root row's and the note; phone is the page's words
	fin := func(step, field, phone, msg, title, line string) {
		ok := step == stepDone
		s.mu.Lock()
		s.ok, s.msg, s.failed = ok, msg, ""
		var kv map[string]string
		if s.ln != nil { // the QR page says it too, keeping the code and PIN for another try
			kv = s.report(step, title, line, msg)
			if !ok {
				s.failed = msg
			}
		}
		s.mu.Unlock()
		if kv != nil {
			s.state.Write(kv)
		}
		answer(ok, field, phone)
	}
	tlsroots.SetDateURL(c.Server) // the ping's certificate check needs a real clock (an unset Q2's isn't)
	mk := s.newClient
	if mk == nil {
		mk = func(c subsonic.Creds) (*subsonic.Client, error) {
			return subsonic.New(c, tlsroots.Client(15*time.Second))
		}
	}
	cl, err := mk(c)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		c, err = cl.SignIn(ctx) // with OpenSubsonic API keys, only the key is kept
	}
	if err != nil {
		field, phone, step, title, line := s.failure(r.Context(), err, host, hostname)
		fin(step, field, phone, subsonic.Message(err, host), title, line)
		return
	}
	if err := creds.Save(s.dir, c); err != nil {
		fin(stepError, "", "Couldn't save the sign-in", "Couldn't save the sign-in", "Couldn't Sign In", "Couldn't save the sign-in")
		return
	}
	u.User = nil // the form's next address: never a password
	q2.WriteFile(filepath.Join(s.dir, lastServerFile), []byte(u.String()+"\n"))
	fin(stepDone, "", "Signed in as "+c.User, "Signed in as "+c.User, "Signed In", c.User+" on "+hostname)
	go s.stop()
}

// capListener closes connections past its semaphore's size at once.
type capListener struct {
	net.Listener
	sem chan struct{}
}

func (l capListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &capConn{Conn: c, sem: l.sem}, nil
		default:
			c.Close()
		}
	}
}

type capConn struct {
	net.Conn
	sem  chan struct{}
	once sync.Once
}

func (c *capConn) Close() error {
	c.once.Do(func() { <-c.sem })
	return c.Conn.Close()
}
