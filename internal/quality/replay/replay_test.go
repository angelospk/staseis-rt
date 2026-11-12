package replay

import (
	"os"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/tools"
)

// TestReplayRealHour rates one real hour of history and logs the level distribution. It needs
// the history (not in the repo), so it runs only with ATRT_REPLAY_HISTORY (a <state>/history
// directory), ATRT_REPLAY_SNAPSHOT (the GTFS snapshot of its feed), ATRT_REPLAY_VERSION and
// ATRT_REPLAY_HOUR (local, 2006-01-02T15). Run with -v to see the numbers.
func TestReplayRealHour(t *testing.T) {
	hist, snap := os.Getenv("ATRT_REPLAY_HISTORY"), os.Getenv("ATRT_REPLAY_SNAPSHOT")
	version, hour := os.Getenv("ATRT_REPLAY_VERSION"), os.Getenv("ATRT_REPLAY_HOUR")
	if hist == "" || snap == "" || version == "" || hour == "" {
		t.Skip("set ATRT_REPLAY_HISTORY, ATRT_REPLAY_SNAPSHOT, ATRT_REPLAY_VERSION and ATRT_REPLAY_HOUR")
	}
	h, err := time.ParseInLocation("2006-01-02T15", hour, gtfs.Athens)
	if err != nil {
		t.Fatal(err)
	}
	f, err := tools.LoadFeed("", snap, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(Config{History: hist, Feed: f, Version: version, Hour: h, Usual: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("training days %d, fixes %d", res.UsualDays, res.Fixes)
	t.Logf("vehicles %v (bunched %d, early %d)", res.Vehicles, res.Bunched, res.Early)
	t.Logf("segments %v", res.Segments)
	t.Logf("%.1f µs per fix, %.1f ms per segment rating, +%.1f MB",
		float64(res.Work.Microseconds())/float64(max(res.Observed, 1)),
		float64(res.SegmentWork.Microseconds())/1000/float64(max(res.SegmentCalls, 1)),
		(float64(res.HeapAfter)-float64(res.HeapBefore))/1e6)
	if res.Fixes == 0 || res.UsualDays == 0 {
		t.Fatal("nothing replayed")
	}
	rated := 0
	for k, n := range res.Vehicles {
		if k != "-/unknown" && k != "headway/unknown" {
			rated += n
		}
	}
	if rated == 0 || res.Segments["normal"] == 0 {
		t.Errorf("no level at all: %v %v", res.Vehicles, res.Segments)
	}
}
