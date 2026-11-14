//go:build realserver

package subsonic

import (
	"net/http"
	"net/url"
	"strings"
)

// NewPlain is New without its https rule, for measuring the app against a server on a private
// network (go test -tags realserver). The tag keeps it out of every build of the app.
func NewPlain(c Creds, hc *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.Server), "/"))
	if err != nil || u.Host == "" {
		return nil, &APIError{Op: "new", Msg: "bad server address", kind: ErrUnreachable}
	}
	return &Client{c: c, base: u, hc: hc}, nil
}
