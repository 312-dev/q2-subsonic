package main

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/andsuor/q2-subsonic/internal/subsonic"
)

// reach is whether the server can be reached, for the Q2's status row and its toasts (Q2 Muse's
// docs/oled-design.md#status-row and #dialogs-and-toasts). Every request the client makes is told
// to seen (subsonic.Client.Seen); the state file says
//
//	reach=down <code>     while the server can't be reached, until a request to it succeeds
//	srverr=<unix> <code>  each failure to toast: the server going out of reach, and a streamed
//	                      song given up (stream.go giveUp) for a server's reason
//
// where <code> is the HTTP status of a proxy answering for a server that isn't there (502, 503,
// 504) or of the failed song, "none" for no answer at all (a refused or timed-out connection, a
// body that stopped) and "dns" for a name that doesn't resolve. The firmware words them "Server
// error 502", "Server not answering" and "Server not found", and toasts each srverr once.
type reach struct {
	mu   sync.Mutex
	down string // the code while out of reach, "" while reachable or not yet known
	keys *stateKeys
	now  func() time.Time
	// wentDown, optional, is told of the error that took the server out of reach (Diagnostics)
	wentDown func(err error)
}

// reachCode is the code err gives the server's reach: "" for an answer from the server itself (a
// Subsonic error, a refused sign-in, a 404 for an endpoint it lacks), which says it is there.
func reachCode(err error) string {
	if subsonic.ConnectionLost(err) {
		if errors.Is(err, subsonic.ErrNoName) {
			return "dns"
		}
		return "none"
	}
	var ae *subsonic.APIError
	if errors.As(err, &ae) && ae.Code == 0 && (ae.HTTP == 502 || ae.HTTP == 503 || ae.HTTP == 504) {
		return strconv.Itoa(ae.HTTP)
	}
	return ""
}

// seen is a request's outcome: nil, a server that answered, clears the mark; one out of reach sets
// it, and toasts once as the server goes.
func (r *reach) seen(err error) {
	code := ""
	if err != nil {
		if code = reachCode(err); code == "" {
			return
		}
	}
	r.mu.Lock()
	if code == r.down {
		r.mu.Unlock()
		return
	}
	went := r.down == ""
	r.down = code
	if code == "" {
		r.keys.setReach("", "")
		r.mu.Unlock()
		return
	}
	toast := ""
	if went {
		toast = fmt.Sprintf("%d %s", r.now().Unix(), code)
	}
	r.keys.setReach("down "+code, toast)
	r.mu.Unlock()
	if went && r.wentDown != nil {
		r.wentDown(err)
	}
}

// gaveUp is a streamed song given up (skipped, or the queue stopped at it): a toast with why, when
// the server is why, its HTTP status for an error the server or a proxy sent (5xx).
func (r *reach) gaveUp(err error) {
	r.seen(err)
	code := reachCode(err)
	var ae *subsonic.APIError
	if code == "" && errors.As(err, &ae) && ae.HTTP >= 500 && ae.HTTP <= 599 {
		code = strconv.Itoa(ae.HTTP)
	}
	if code == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys.setReach(r.keys.reachNow(), fmt.Sprintf("%d %s", r.now().Unix(), code))
}
