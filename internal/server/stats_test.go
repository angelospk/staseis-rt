package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatsEndpoints(t *testing.T) {
	a := newApp(t, time.Unix(1_800_000_000, 0))
	if res, _ := get(t, a, "/v1/stats"); res.StatusCode != 404 {
		t.Fatalf("no index: %d", res.StatusCode)
	}
	days := filepath.Join(a.statsDir(), "days")
	os.MkdirAll(days, 0o755)
	os.WriteFile(filepath.Join(a.statsDir(), "index.json"), []byte(`{"days":["2026-10-30"]}`), 0o644)
	os.WriteFile(filepath.Join(days, "2026-10-30.json"), []byte(`{"date":"2026-10-30"}`), 0o644)

	res, body := get(t, a, "/v1/stats")
	if res.StatusCode != 200 || string(body) != `{"days":["2026-10-30"]}` || maxAge(t, res) != statsIndexMaxAge {
		t.Fatalf("index: %d %s", res.StatusCode, body)
	}
	res, body = get(t, a, "/v1/stats/days/2026-10-30")
	if res.StatusCode != 200 || string(body) != `{"date":"2026-10-30"}` || maxAge(t, res) != statsDayMaxAge ||
		res.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("day: %d %s", res.StatusCode, body)
	}
	if res, _ := get(t, a, "/v1/stats/days/2026-10-31"); res.StatusCode != 404 {
		t.Fatalf("missing day: %d", res.StatusCode)
	}
	// Days before stats.FirstDay are not served, even when their file is there.
	os.WriteFile(filepath.Join(days, "2026-10-29.json"), []byte(`{"date":"2026-10-29"}`), 0o644)
	if res, body := get(t, a, "/v1/stats/days/2026-10-29"); res.StatusCode != 404 {
		t.Fatalf("early day: %d %s", res.StatusCode, body)
	}
	for _, p := range []string{"/v1/stats/days/2026-1-31", "/v1/stats/days/index", "/v1/stats/days/2026-10-30.json"} {
		if res, _ := get(t, a, p); res.StatusCode != 400 {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
	}
}

func TestStatsPlanUsesTheFeedInUse(t *testing.T) {
	a := newApp(t, time.Unix(1_800_000_000, 0))
	p, err := a.statsPlan("2026-09-28") // a Monday of the test feed
	if err != nil || p.GTFS == "" || p.GTFS != worldVersion(a.world()) || len(p.Trips) != 6 {
		t.Fatalf("plan = %+v, err %v", p, err)
	}
	if p.GTFSHeadways {
		t.Error("Athens: GTFS headways on")
	}
	a.city, _ = CityByID("thessaloniki") // no daily schedule: headways from the GTFS trips (#164)
	if p, err := a.statsPlan("2026-09-28"); err != nil || !p.GTFSHeadways {
		t.Fatalf("Thessaloniki: plan = %+v, err %v", p, err)
	}
}
