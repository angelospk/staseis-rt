package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	qreplay "github.com/angelospk/athens-transit-rt/internal/quality/replay"
	"github.com/angelospk/athens-transit-rt/internal/tools"
)

// qualityReplay rates one hour of the fix history with the live quality (docs/QUALITY.md) and
// prints the level distribution and the cost. No OASA call.
func qualityReplay(args []string) error {
	fs := flag.NewFlagSet("quality-replay", flag.ExitOnError)
	hist := fs.String("history", "", "directory of the hourly history archives (<state>/history)")
	snap := fs.String("snapshot", "", "GTFS snapshot of the history's feed")
	gtfsZip := fs.String("gtfs", "", "GTFS zip instead of --snapshot")
	version := fs.String("version", "", "GTFS version of the history rows (the gtfs column)")
	hour := fs.String("hour", "", "local hour to replay, YYYY-MM-DDTHH (Europe/Athens)")
	usualDir := fs.String("usual", "", "directory for the usual day files (default: a temp dir)")
	fs.Parse(args)
	if *hist == "" || *version == "" || *hour == "" {
		return fmt.Errorf("--history, --version and --hour are required")
	}
	h, err := time.ParseInLocation("2006-01-02T15", *hour, gtfs.Athens)
	if err != nil {
		return err
	}
	f, err := tools.LoadFeed(*gtfsZip, *snap, nil)
	if err != nil {
		return err
	}
	if *usualDir == "" {
		d, err := os.MkdirTemp("", "atrt-usual-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(d)
		*usualDir = d
	}
	start := time.Now()
	res, err := qreplay.Run(qreplay.Config{History: *hist, Feed: f, Version: *version, Hour: h, Usual: filepath.Clean(*usualDir)})
	if err != nil {
		return err
	}
	fmt.Printf("hour %s, %d training days, built and replayed in %s\n", h.Format("2006-01-02 15:04 MST"), res.UsualDays,
		time.Since(start).Round(time.Second))
	print := func(title string, m map[string]int) {
		total := 0
		var keys []string
		for k, n := range m {
			keys = append(keys, k)
			total += n
		}
		sort.Strings(keys)
		fmt.Printf("%s (%d):\n", title, total)
		for _, k := range keys {
			fmt.Printf("  %-22s %7d  %5.1f%%\n", k, m[k], 100*float64(m[k])/float64(max(total, 1)))
		}
	}
	print("vehicle fixes", res.Vehicles)
	fmt.Printf("  bunched %d, early %d\n", res.Bunched, res.Early)
	print("segments (every 5 min)", res.Segments)
	fmt.Printf("cost: %d fixes in %s (%.1f µs/fix); segments %s over %d ratings (%.1f ms each)\n",
		res.Observed, res.Work.Round(time.Millisecond), float64(res.Work.Microseconds())/float64(max(res.Observed, 1)),
		res.SegmentWork.Round(time.Millisecond), res.SegmentCalls, float64(res.SegmentWork.Microseconds())/1000/float64(max(res.SegmentCalls, 1)))
	fmt.Printf("heap: +%.1f MB for the tracker and caches (%d vehicles, %d passes, %d traversals; %d stops looked up)\n",
		(float64(res.HeapAfter)-float64(res.HeapBefore))/1e6, res.TrackerSize[0], res.TrackerSize[1], res.TrackerSize[2], res.UsualStops)
	return nil
}
