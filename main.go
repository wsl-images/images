package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

const manifestURL = "https://raw.githubusercontent.com/microsoft/WSL/master/distributions/DistributionInfo.json"

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	list := flag.Bool("matrix", false, "print the current AMD64 build matrix as JSON (no Docker or publishing)")
	distro := flag.String("distro", "", "distribution name to build, or comma-separated names to filter the matrix")
	entryJSON := flag.String("entry", "", "build an exact JSON entry emitted by --matrix instead of fetching upstream again")
	push := flag.Bool("push", false, "publish images; by default only build and smoke-test locally")
	owner := flag.String("owner", "wsl-images", "GHCR namespace")
	quay := flag.String("quay-repository", "", "optional Quay repository, e.g. quay.io/wsl-images/images")
	flag.Parse()
	if flag.NArg() != 0 || (*list && *entryJSON != "") {
		return errors.New("unexpected arguments or incompatible --matrix and --entry flags")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Minute}
	var entries []Entry
	if *entryJSON != "" {
		var entry Entry
		if err := json.Unmarshal([]byte(*entryJSON), &entry); err != nil {
			return fmt.Errorf("parse build entry: %w", err)
		}
		if err := entry.validate(); err != nil {
			return err
		}
		entries = []Entry{entry}
	} else {
		if !*list && (*distro == "" || strings.Contains(*distro, ",")) {
			return errors.New("choose one --distro to build, or use --matrix to list available distributions")
		}
		var err error
		entries, err = fetchEntries(ctx, client, manifestURL, *distro)
		if err != nil {
			return err
		}
	}
	if *list {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Include []Entry `json:"include"`
		}{entries})
	}
	return build(ctx, client, entries[0], buildOptions{
		Owner: strings.ToLower(*owner), Quay: *quay, Push: *push,
		Revision: os.Getenv("GITHUB_SHA"), Now: time.Now().UTC(),
	}, runDocker)
}
