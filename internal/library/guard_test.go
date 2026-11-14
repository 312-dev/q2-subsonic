package library

import "testing"

// A picture in _explaylist_data is the app's only when recorded in PlaylistArt.
func TestOwnedPlaylistArt(t *testing.T) {
	root := t.TempDir()
	mine := root + "/_explaylist_data/Mix.jpg"
	m := &Manifest{Playlists: map[string]string{"Mix": root + "/_explaylist_data/Mix.m3u"},
		PlaylistArt: map[string]Art{"Mix": {File: mine}}}
	if !Owned(root, mine, m) || Owned(root, mine, nil) || Owned(root, root+"/_explaylist_data/Other.jpg", m) ||
		Owned(root, root+"/_explaylist_data/Mix.png", m) {
		t.Fatal("ownership of playlist pictures")
	}
	if err := m.WriteArt(root, root+"/_explaylist_data/Other.png", []byte("x")); err == nil {
		t.Fatal("wrote an unrecorded picture")
	}
}
