package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/histrelease"
	"github.com/angelospk/athens-transit-rt/internal/stats"
)

// history-release: upload the finished days of the citybus.gr fix history as GitHub Release
// assets, once (staseis-rt#152; a daily timer on the VPS runs it). The token comes from
// GITHUB_TOKEN only, never a flag, so it is not in the process list.
func historyReleaseCmd(args []string) error {
	fs := flag.NewFlagSet("history-release", flag.ExitOnError)
	hist := fs.String("history", "/var/lib/atrt/history", "fix history directory (read only)")
	repo := fs.String("repo", "angelospk/staseis-rt", "GitHub repository of the releases")
	api := fs.String("api", "https://api.github.com", "GitHub API base URL")
	fs.Parse(args)
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return errors.New("history-release: GITHUB_TOKEN is not set")
	}
	loc, err := time.LoadLocation(stats.TZ)
	if err != nil {
		return err
	}
	u := &histrelease.Uploader{API: *api, Repo: *repo, Token: token, Client: &http.Client{Timeout: 5 * time.Minute}}
	days, err := histrelease.Run(u, *hist, loc, time.Now())
	for _, d := range days {
		fmt.Println("uploaded", d)
	}
	return err
}
