package subsonic

import (
	"errors"
	"net"
	"net/url"
	"testing"
)

// A name that doesn't resolve is ErrNoName, and still a lost connection with its message as any
// unreachable server's; any other DNS failure (a resolver that timed out) is plain ErrUnreachable.
func TestNoName(t *testing.T) {
	nx := &url.Error{Op: "Get", URL: "https://music.example", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "music.example", IsNotFound: true}}}
	if k := transportKind(nx); k != ErrNoName {
		t.Fatalf("kind %v, want ErrNoName", k)
	}
	err := &APIError{Op: "ping", kind: transportKind(nx)}
	if !ConnectionLost(err) || !errors.Is(err, ErrUnreachable) || Message(err, "music.example") != "Can't reach music.example" {
		t.Fatalf("lost %v, message %q", ConnectionLost(err), Message(err, "music.example"))
	}
	slow := &url.Error{Op: "Get", URL: "https://music.example", Err: &net.DNSError{Err: "i/o timeout", IsTimeout: true}}
	if k := transportKind(slow); k != ErrUnreachable {
		t.Fatalf("a resolver timeout's kind %v", k)
	}
}
