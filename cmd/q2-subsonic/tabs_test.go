package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The app has no sections and no browse rows (the user's, 2026-11-19: the server's music is in the
// Q2's Library): on firmware with "sections" too, the root is Downloads, Sync and Settings, whatever
// the argument, and no row is a section.
func TestRootHasNoSections(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 3, songsPer: 1}, "pages window sort sections", true)
	for _, args := range [][]string{nil, {"home"}, {"albums"}, {"discover"}} {
		r := d.do("menu", args...)
		if r.Message != "refresh" || strings.Join(titles(r), "|") != "Downloads|Sync|Settings" {
			t.Errorf("menu %v: %q, message %q", args, titles(r), r.Message)
		}
	}
}

// Signed out, the root is the sign-in row whatever the argument.
func TestRootSignedOutAnyArgument(t *testing.T) {
	d := newTestDaemon(t, &nav{}, "sections", false)
	for _, args := range [][]string{nil, {"home"}, {"albums"}} {
		r := d.do("menu", args...)
		if r.Message != "refresh" || r.Status != "" || len(r.Rows) != 1 || r.Rows[0].URI != "signin" {
			t.Errorf("menu %v: %+v", args, r)
		}
	}
}

// The first signed-in root clears the QR page's state.
func TestRootClearsSigninState(t *testing.T) {
	d := newTestDaemon(t, &nav{albums: 1, songsPer: 1}, "", true)
	file := filepath.Join(d.tmp, "q2-subsonic.state")
	d.state.Write(map[string]string{"note": "Signed in"})
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "note=") {
		t.Fatalf("state %q", b)
	}
	d.do("menu")
	if b, _ := os.ReadFile(file); strings.Contains(string(b), "note=") {
		t.Errorf("state kept %q", b)
	}
}
