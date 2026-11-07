package server

import (
	"context"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
)

// prefetchDaily fetches OASA's daily timetable of every line once a day, from 03:00 (after the
// night runs), through background slots: a stop with many lines then finds them all cached. A
// line that failed is asked again on demand (dailyNegativeTTL), not by the next round. Once the
// day is fetched, every round saves what the cache has of it to the archive (saveDailyArchive).
func (a *App) prefetchDaily(ctx context.Context) {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	done := ""
	for {
		now := a.now()
		day := gtfs.Midnight(now).Format(time.DateOnly)
		if day != done && now.In(gtfs.Athens).Hour() >= 3 && len(a.meta.LineIDs()) > 0 {
			start := time.Now()
			if a.prefetchDailyOnce(ctx) {
				done = day
				a.prefetched.Store(day)
				a.log.Info("daily schedule: prefetched", "lines", len(a.meta.LineIDs()), "took", time.Since(start).Round(time.Second))
			}
		}
		if day == done {
			if _, err := a.saveDailyArchive(gtfs.Midnight(now)); err != nil {
				a.log.Warn("daily schedule: archive", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// prefetchDailyOnce fetches today's timetable of each line not cached yet, one line at a time.
// false when ctx ended or the day changed first.
func (a *App) prefetchDailyOnce(ctx context.Context) bool {
	today := gtfs.Midnight(a.now())
	for _, line := range a.meta.LineIDs() {
		if ctx.Err() != nil || !gtfs.Midnight(a.now()).Equal(today) { // past midnight: the next round
			return false
		}
		a.dailyVia(ctx, a.canonLine(line), today, a.dailyBg, dailyPrefetchTimeout, false)
	}
	return ctx.Err() == nil
}
