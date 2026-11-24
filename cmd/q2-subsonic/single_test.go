package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A second copy of the app started while one runs (a Q2 Pod boot that resumed a stream started two
// launchers at once) exits at once with status 0, leaving the first copy and its socket alone; once
// the first has ended, a new copy runs.
func TestASecondCopyExitsAndTheFirstRunsOn(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the app")
	}
	work := t.TempDir()
	bin := filepath.Join(work, "q2-subsonic")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dir, tmp := filepath.Join(work, "card", ".subsonic"), filepath.Join(work, "tmp")
	for _, d := range []string{dir, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(tmp, "q2-subsonic.browse.sock")
	start := func() *exec.Cmd {
		cmd := exec.Command(bin, "-dir", dir, "-card", filepath.Join(work, "card"), "-tmp", tmp)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	listening := func(what string) {
		for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(sock); err == nil {
				return
			}
			if time.Now().After(end) {
				t.Fatalf("%s: no socket after 10 s", what)
			}
		}
	}
	first := start()
	defer first.Process.Kill()
	listening("the first copy")
	before, _ := os.Stat(sock)
	second := start()
	done := make(chan error, 1)
	go func() { done <- second.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second copy: %v, want exit 0", err)
		}
	case <-time.After(3 * time.Second):
		second.Process.Kill()
		<-done
		t.Fatal("the second copy still runs 3 s on: two copies at once")
	}
	if after, err := os.Stat(sock); err != nil || !os.SameFile(before, after) {
		t.Fatalf("the first copy's socket: %v, replaced %v", err, err == nil)
	}
	if err := first.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the first copy: %v", err)
	}
	first.Process.Signal(syscall.SIGTERM)
	first.Wait()
	third := start()
	defer third.Process.Kill()
	listening("a copy after the first ended")
	third.Process.Signal(syscall.SIGTERM)
	third.Wait()
}
