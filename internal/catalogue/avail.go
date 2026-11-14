package catalogue

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/andsuor/q2-subsonic/internal/library"
)

// Avail appends a line to catalogue.avail in dir: "<sid>\t<path>" when the stream cache has a whole
// copy of the song at path, "-<sid>" (path "") when it no longer has one. The file is emptied each
// time a catalogue is written (Write); between two, the firmware reads what was added since.
func Avail(card, dir, sid, path string) error {
	p := filepath.Join(dir, AvailName)
	if !library.Owned(card, p, nil) {
		return errors.New("refusing path outside owned trees: " + p)
	}
	line := fields("-" + sid)
	if path != "" {
		line = fields(sid, path)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
