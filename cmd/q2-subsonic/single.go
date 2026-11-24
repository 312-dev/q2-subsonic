package main

import (
	"errors"
	"os"
	"syscall"
)

// oneCopy is the open lock file of the copy that holds it, kept for the life of the process: the
// kernel drops the lock when the process ends, however it ends.
var oneCopy *os.File

// lockOne takes the app's lock on path (flock, exclusive, not waiting). false: another copy holds
// it, and this one must exit before it touches the card or /tmp, as two copies would each write the
// same state, queue and cache files. An error (no /tmp to write, say) lets the app run unguarded.
func lockOne(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	oneCopy = f
	return true, nil
}
