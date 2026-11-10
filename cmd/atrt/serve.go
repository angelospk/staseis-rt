package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/citybus"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/oseth"
	"github.com/angelospk/athens-transit-rt/internal/sched"
	"github.com/angelospk/athens-transit-rt/internal/server"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8095", "HTTP listen address")
	metricsListen := fs.String("metrics-listen", "127.0.0.1:8096", "loopback address for GET /metrics (empty = off)")
	state := fs.String("state", "/var/lib/atrt", "state directory (snapshot, telematics metadata)")
	rps := fs.Float64("rps", telematics.DefaultRPS, fmt.Sprintf("OASA requests per second, whole process (max %d)", telematics.MaxRPS))
	matcher := fs.String("matcher", "memory", "trip matcher: memory, hungarian or greedy")
	releaseURL := fs.String("release-url", "", "snapshot release base URL (default: this repo's gtfs-snapshot release)")
	gtfsZip := fs.String("gtfs", "", "load this GTFS zip instead of the release snapshot (local use)")
	gtfsCKAN := fs.String("gtfs-ckan", "", "CKAN package_show URL of the GTFS (default: the city's dataset; Thessaloniki only)")
	telURL := fs.String("telematics-url", "", "OASA telematics API base URL")
	osethRPS := fs.Float64("oseth-rps", 1, fmt.Sprintf("OSETH (Thessaloniki) requests per second, own budget (max %d, 0 = off)", oseth.MaxRPS))
	stasyURL := fs.String("stasy-url", "", "STASY (metro, tram) timetable catalogue (default: the Growthfund CKAN dataset)")
	suburbanURL := fs.String("suburban-url", "", "railway.gov.gr base URL for Proastiakos boards")
	mpassURL := fs.String("mpass-url", "", "ose.mpass.ltd API base URL (Proastiakos board fallback)")
	proxy := fs.String("proxy", "", "proxy for OASA requests, e.g. socks5h://127.0.0.1:40001 (empty = direct)")
	check := fs.Duration("release-check", 30*time.Minute, "how often to look for a new snapshot")
	hist := fs.Bool("history", false, "keep every new GPS fix in <state>/history (hourly CSV, gzipped)")
	histMB := fs.Int64("history-max-mb", 500, "fix history: delete the oldest hours above this size")
	histDays := fs.Int("history-days", 30, "fix history: delete hours older than this")
	cityID := fs.String("city", "athens", "athens, thessaloniki or citybus")
	cities := fs.String("cities", "all", "citybus only: \"all\" or codes/names separated by commas (agq,her,...)")
	fs.Parse(args)

	var histBytes int64
	if *hist {
		if *histMB <= 0 || *histDays <= 0 {
			return fmt.Errorf("--history-max-mb and --history-days must be positive")
		}
		histBytes = *histMB << 20
	}
	histKeep := time.Duration(*histDays) * 24 * time.Hour
	if *cityID == "citybus" {
		return serveCitybus(*listen, *state, *cities, histBytes, histKeep)
	}

	city, err := server.CityByID(*cityID)
	if err != nil {
		return fmt.Errorf("bad --city: %w", err)
	}
	var salt string
	if city.ID == "thessaloniki" {
		// The API token's salt is a secret, so it comes from the environment, not a flag.
		if salt = os.Getenv("OASTH_TOKEN_SALT"); salt == "" {
			return fmt.Errorf("thessaloniki needs OASTH_TOKEN_SALT")
		}
	}

	kind, ok := match.ParseKind(*matcher)
	if !ok {
		return fmt.Errorf("unknown matcher %q", *matcher)
	}
	if *proxy != "" {
		u, err := url.Parse(*proxy)
		if err != nil || u.Host == "" || (u.Scheme != "socks5" && u.Scheme != "socks5h" && u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("bad --proxy %q", *proxy)
		}
	}
	if *rps > telematics.MaxRPS {
		return fmt.Errorf("--rps %.1f is above the maximum of %d", *rps, telematics.MaxRPS)
	}
	if *osethRPS < 0 || *osethRPS > oseth.MaxRPS {
		return fmt.Errorf("--oseth-rps %.1f is outside 0..%d", *osethRPS, oseth.MaxRPS)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	server.UseCityGlobals(city)
	app := server.New(server.Config{Listen: *listen, MetricsListen: *metricsListen, StateDir: *state, RPS: *rps, Matcher: kind,
		ReleaseURL: *releaseURL, GTFSZip: *gtfsZip, GTFSCKANURL: *gtfsCKAN, TelematicsURL: *telURL, OSETHRPS: *osethRPS, City: city.ID, OASTHSalt: salt, Proxy: *proxy, ReleaseCheck: *check, HistoryMaxBytes: histBytes,
		STASYURL: *stasyURL, SuburbanURL: *suburbanURL, MPassURL: *mpassURL,
		Sched: sched.DefaultConfig(), HistoryKeep: histKeep}, log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = app.Run(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// serveCitybus runs every citybus.gr city given in one process: /{city}/v1/..., /{city}/v2/...
func serveCitybus(listen, state, cities string, histBytes int64, histKeep time.Duration) error {
	ids, err := citybus.ParseCities(cities)
	if err != nil {
		return fmt.Errorf("bad --cities: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	svc, err := citybus.New(citybus.Config{StateDir: state, Cities: ids, HistoryMaxBytes: histBytes, HistoryKeep: histKeep}, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return svc.Run(ctx, listen)
}
