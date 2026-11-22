// q2-subsonic is a Q2 Pod card app (contrib/shanlingq2/README): a Navidrome (Subsonic API) client for
// the Shanling Q2 that syncs and streams. It runs from the card's .subsonic folder (copied to /tmp
// first, so the card is never kept busy) and draws the Q2's Subsonic page over Q2SPOT/1.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

const version = "q2-v1.0"

// build is the commit the app was built from, "-dirty" after it when the tree had changes, set by
// contrib/shanlingq2/build.sh (-ldflags -X main.build=...); "" for a plain go build.
var build string

// versionBuild is version with its build: what the log, Settings' Version row and the state file's
// build= say, so a Q2 tells which app it runs (every zip of a version said the same before).
func versionBuild() string {
	if build == "" {
		return version
	}
	return version + "+" + build
}

func main() {
	dir := flag.String("dir", "/mnt/mmc/.subsonic", "the add-on's folder on the card")
	card := flag.String("card", "/mnt/mmc", "the card's root")
	tmp := flag.String("tmp", "/tmp", "where the firmware's files and the socket live")
	flag.Parse()
	// The Q2 has about 67 MB beside demo and no swap: the GC works harder near this instead of the
	// kernel's OOM killer picking the app, or demo (I16).
	debug.SetMemoryLimit(48 << 20)
	nice := lowerPriority()
	q2.SetupLog("q2-subsonic")
	q2.Card = *card
	log.Print("start ", versionBuild())
	d := newDaemon(*dir, *card, *tmp)
	d.log.Printf("start %s", versionBuild())
	if nice != nil {
		d.log.Printf("nice: %v", nice)
	}
	sock := filepath.Join(*tmp, "q2-subsonic.browse.sock")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; d.close(sock); os.Exit(0) }()
	log.Fatal(d.serve(sock))
}

// appNice is the app's nice value: its work (covers, a walk, songs fetched ahead) yields the CPU to
// the firmware's player, Bluetooth audio and UI whenever they want it.
const appNice = 10

// lowerPriority sets every thread of the app to appNice. Linux keeps a nice value per thread, and a
// thread the Go runtime starts later takes its starter's, so setting the threads there now covers
// all of them.
func lowerPriority() error {
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return err
	}
	for _, e := range ents {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, appNice); err != nil {
				return err
			}
		}
	}
	return nil
}
