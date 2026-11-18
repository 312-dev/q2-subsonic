package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

// resolve cleans p and resolves symlinks in its directory (deepest existing ancestor), leaving
// the last element alone so a symlink is judged, and removed, as itself.
func resolve(p string) string {
	p = filepath.Clean(p)
	dir, base := filepath.Split(p)
	dir = filepath.Clean(dir)
	var rest []string
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		rest = append([]string{filepath.Base(dir)}, rest...)
		dir = parent
	}
	return filepath.Join(append([]string{dir}, append(rest, base)...)...)
}

// Owned reports whether path is strictly inside what the app may touch under root: Subsonic/,
// .subsonic/, and in _explaylist_data/ only the .m3u files recorded in m.Playlists and the pictures
// recorded in m.PlaylistArt (m may be nil), plus legacy "Navidrome - *.m3u" files, which exist only
// to be migrated away.
func Owned(root, path string, m *Manifest) bool {
	parts := relParts(root, path)
	if parts == nil {
		return false
	}
	switch parts[0] {
	case "Subsonic", ".subsonic":
		return len(parts) > 1
	case "_explaylist_data":
		if len(parts) != 2 {
			return false
		}
		if m != nil && ValidArtFile(root, path) {
			for _, a := range m.PlaylistArt {
				if filepath.Clean(a.File) == filepath.Clean(path) {
					return true
				}
			}
			return false
		}
		if !strings.HasSuffix(parts[1], ".m3u") {
			return false
		}
		if strings.HasPrefix(parts[1], "Navidrome - ") {
			return true
		}
		if m != nil {
			for _, f := range m.Playlists {
				if filepath.Clean(f) == filepath.Clean(path) {
					return true
				}
			}
		}
	}
	return false
}

// relParts splits path, symlinks resolved, relative to root; nil when it is root or outside it.
func relParts(root, path string) []string {
	rel, err := filepath.Rel(resolve(root+"/."), resolve(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return strings.Split(rel, string(filepath.Separator))
}

// InSongs reports whether path, in Clean form, is strictly inside <root>/Subsonic/: the only place
// a sync writes songs. Owned also allows .subsonic/, which holds the app binary and run script.
func InSongs(root, path string) bool {
	parts := relParts(root, path)
	return filepath.Clean(path) == path && len(parts) > 1 && parts[0] == "Subsonic"
}

// ValidPlaylistFile reports whether f is exactly <root>/_explaylist_data/<name>.m3u in Clean form,
// the only shape a recorded playlist path may have.
func ValidPlaylistFile(root, f string) bool {
	b := filepath.Base(f)
	return filepath.Clean(f) == f && filepath.Dir(f) == filepath.Join(root, "_explaylist_data") &&
		strings.HasSuffix(b, ".m3u") && b != ".m3u"
}

// ValidArtFile reports whether f is exactly <root>/_explaylist_data/<name>.jpg or .png in Clean
// form, the only shape a recorded playlist picture may have.
func ValidArtFile(root, f string) bool {
	b, ext := filepath.Base(f), filepath.Ext(f)
	return filepath.Clean(f) == f && filepath.Dir(f) == filepath.Join(root, "_explaylist_data") &&
		(ext == ".jpg" || ext == ".png") && b != ext
}

// ArtFile is the picture beside playlist file f (a .m3u) that Q2 Pod shows for it: the playlist's
// name is the file's, so the picture is that name with ext.
func ArtFile(f, ext string) string { return strings.TrimSuffix(f, filepath.Ext(f)) + ext }

// WriteArt writes a playlist picture recorded in m.PlaylistArt, atomically.
func (m *Manifest) WriteArt(root, path string, data []byte) error {
	if err := checkOwned(root, path, m); err != nil {
		return err
	}
	return q2.WriteFile(path, data)
}

// checkOwned is the gate for every write and delete: path must already be in Clean form (so the
// OS and the guard read the same path, e.g. no "link/.." tricks) and Owned.
func checkOwned(root, path string, m *Manifest) error {
	if filepath.Clean(path) != path || !Owned(root, path, m) {
		return errors.New("refusing path outside owned trees: " + path)
	}
	return nil
}

// WriteOwned writes a file of the app's own atomically, refusing a path that is not Owned.
func WriteOwned(root, path string, data []byte) error {
	if err := checkOwned(root, path, nil); err != nil {
		return err
	}
	return q2.WriteFile(path, data)
}

// Remove is the only way the app deletes: it refuses anything not Owned, treats a missing file
// as done, and prunes folders left empty under Subsonic/ (never Subsonic/ itself). A song's lyrics
// beside it (LrcFile) go with it.
func Remove(root, path string) error { return remove(root, path, nil) }

// LrcFile is the lyrics file Q2 Pod reads for the song at path: its name with .lrc for the
// extension, beside it.
func LrcFile(path string) string { return strings.TrimSuffix(path, filepath.Ext(path)) + ".lrc" }

// IsSong reports whether name, a file's, is a song's by its extension (Playable).
func IsSong(name string) bool { return Playable(strings.TrimPrefix(filepath.Ext(name), ".")) }

// Remove is the package Remove that also accepts the playlist files recorded in m.
func (m *Manifest) Remove(root, path string) error { return remove(root, path, m) }

func remove(root, path string, m *Manifest) error {
	if err := checkOwned(root, path, m); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if IsSong(path) {
		os.Remove(LrcFile(path)) // Owned: beside path, in its folder
	}
	top := filepath.Join(resolve(root+"/."), "Subsonic")
	for d := filepath.Dir(resolve(path)); strings.HasPrefix(d, top+string(filepath.Separator)); d = filepath.Dir(d) {
		if os.Remove(d) != nil { // fails while non-empty
			break
		}
	}
	return nil
}

// Touch sets the time of a file of the app's own to t, when t is later than the one it has: a
// zero t (the clock isn't known) or an earlier one changes nothing.
func Touch(root, path string, t time.Time) error {
	if err := checkOwned(root, path, nil); err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil || t.IsZero() || !t.After(st.ModTime()) {
		return err
	}
	return os.Chtimes(path, t, t)
}
