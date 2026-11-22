// Package applog is a small append-per-line log that never keeps its file open, so the card is
// never busy, and that rotates to <path>.1 at a size limit. Every line is redacted (Redact) on its
// way in, and Scrub redacts what an older build wrote.
package applog

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"
)

type Log struct {
	path string
	max  int64
	mu   sync.Mutex
}

// Open returns a log at path that rotates once the file would pass max bytes.
func Open(path string, max int64) *Log { return &Log{path: path, max: max} }

// Printf appends one timestamped line, opening and closing the file each time.
func (l *Log) Printf(format string, args ...any) {
	line := time.Now().UTC().Format("2006-01-02 15:04:05 ") + Redact(fmt.Sprintf(format, args...)) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, err := os.Stat(l.path); err == nil && st.Size()+int64(len(line)) > l.max {
		os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	f.WriteString(line)
	f.Close()
}

// Scrub redacts the log and its rotated <path>.1 in place, for lines an older build wrote before
// Redact covered all they hold. A file that changes is rewritten whole, a .tmp renamed over it, so
// a power cut leaves the old file or the new one; one with nothing to redact is not written.
func (l *Log) Scrub() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var first error
	for _, p := range []string{l.path, l.path + ".1"} {
		if err := scrub(p); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ScrubOnce is Scrub the first time only: a marker beside the log (<path>.scrubbed) says it ran.
// Printf redacts every line it writes, so only an older build's lines need it, and a start that
// read and matched 2 MB of log before serving took seconds on the Q2 (66 ms on a desktop).
func (l *Log) ScrubOnce() error {
	mark := l.path + ".scrubbed"
	if _, err := os.Stat(mark); err == nil {
		return nil
	}
	if err := l.Scrub(); err != nil {
		return err
	}
	return os.WriteFile(mark, nil, 0o600)
}

func scrub(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	out := []byte(Redact(string(b)))
	if bytes.Equal(out, b) {
		return nil
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(out)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

var (
	secret   = regexp.MustCompile(`(?i)([?&](?:u|p|t|s|password|apikey)=)[^&\s"]*`)
	userinfo = regexp.MustCompile(`(://[^/@\s":]*:)[^/\s"]*@`)
)

// Redact replaces the values of the query parameters u, p, t, s, password and apiKey (the user name,
// the password, the salted token and its salt, and the API key a Subsonic request signs in with),
// and the password in a URL's user:password@, with ***.
func Redact(s string) string {
	return userinfo.ReplaceAllString(secret.ReplaceAllString(s, "${1}***"), "${1}***@")
}
