package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A press (.catplay renamed into place, as the firmware writes it) kicks the tick at once; another
// file in the folder doesn't.
func TestAPressKicksTheTickAtOnce(t *testing.T) {
	dir := t.TempDir()
	kick := make(chan struct{}, 1)
	go watchNames(dir, []string{"q2-subsonic.catplay"}, kick)
	time.Sleep(50 * time.Millisecond) // the watch in place
	os.WriteFile(filepath.Join(dir, "other.tmp"), []byte("x"), 0o644)
	os.Rename(filepath.Join(dir, "other.tmp"), filepath.Join(dir, "q2-other.state"))
	select {
	case <-kick:
		t.Fatal("kicked by another file")
	case <-time.After(100 * time.Millisecond):
	}
	os.WriteFile(filepath.Join(dir, "c.tmp"), []byte("7 0\n/x\n"), 0o644)
	at := time.Now()
	os.Rename(filepath.Join(dir, "c.tmp"), filepath.Join(dir, "q2-subsonic.catplay"))
	select {
	case <-kick:
		if d := time.Since(at); d > 100*time.Millisecond {
			t.Fatalf("kicked %v after the press", d)
		}
	case <-time.After(time.Second):
		t.Fatal("not kicked by the press")
	}
}
