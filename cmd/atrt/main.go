// Command atrt is a GTFS-Realtime producer and live JSON API for OASA buses and trolleys.
//
//	atrt serve     poll OASA and serve /v1/* (the VPS service)
//	atrt snapshot  build the static snapshot and map data from the GTFS zip (GitHub Actions)
//	atrt compare   compare our ETAs with OASA's own predictions, once
//	atrt record    record GPS fixes, matches and OASA ETAs to SQLite
//	atrt replay    score the matchers on a recording (--blocks: check GTFS vehicle blocks)
//	atrt stats     sum the fix history into daily statistics, once
//	atrt history-release  upload the finished days of the citybus.gr fix history to GitHub Releases, once
package main

import (
	"fmt"
	"os"
)

var commands = map[string]func(args []string) error{
	"serve":           serve,
	"snapshot":        snapshot,
	"compare":         compare,
	"record":          record,
	"replay":          replay,
	"stats":           statsCmd,
	"history-release": historyReleaseCmd,
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: atrt serve|snapshot|compare|record|replay|stats|history-release [flags]   (atrt <command> -h for flags)")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "atrt:", err)
		os.Exit(1)
	}
}
