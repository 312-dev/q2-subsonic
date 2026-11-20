package main

import (
	"fmt"
	"time"
)

// Background jobs say in log.txt when they start and how long they took (the user's report,
// 2026-11-19: the app at 80 to 104% CPU twice ten minutes apart with nothing in its log but pings).
// A job that runs every few seconds or minutes logs only when it did work worth seeing:
// jobQuiet is how long such a job may take and stay unlogged.
const jobQuiet = 200 * time.Millisecond

// job logs name's start and returns its end, which logs how long it took and what it did. quiet
// jobs log neither unless they took jobQuiet or more.
func (d *daemon) job(name string, quiet bool) func(format string, a ...any) {
	start := time.Now()
	if !quiet {
		d.log.Printf("job %s: start", name)
	}
	return func(format string, a ...any) {
		took := time.Since(start)
		if quiet && took < jobQuiet {
			return
		}
		d.log.Printf("job %s: %s in %v", name, fmt.Sprintf(format, a...), took.Round(time.Millisecond))
	}
}
