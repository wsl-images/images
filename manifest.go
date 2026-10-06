package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

type URLInfo struct {
	URL    string `json:"Url"`
	SHA256 string `json:"Sha256"`
}

type Distribution struct {
	Name  string  `json:"Name"`
	AMD64 URLInfo `json:"Amd64Url"`
}

type Entry struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

var imageComponent = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
var dockerTag = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)

func normalizeSHA(value string) (string, error) {
	value = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "0x")
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("invalid SHA256 %q", value)
	}
	return value, nil
}

func (entry *Entry) validate() error {
	if entry.Name == "" || entry.Image != strings.ToLower(entry.Name) || !imageComponent.MatchString(entry.Image) {
		return fmt.Errorf("invalid distribution/image name %q/%q", entry.Name, entry.Image)
	}
	u, err := url.Parse(entry.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("%s: download URL must use HTTPS", entry.Name)
	}
	entry.SHA256, err = normalizeSHA(entry.SHA256)
	return err
}

func parseManifest(reader io.Reader, filter string) ([]Entry, error) {
	var manifest struct {
		Modern map[string][]Distribution `json:"ModernDistributions"`
	}
	if err := json.NewDecoder(io.LimitReader(reader, 4<<20)).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("parse distribution manifest: %w", err)
	}
	wanted := map[string]bool{}
	if filter != "" {
		for name := range strings.SplitSeq(filter, ",") {
			wanted[strings.ToLower(strings.TrimSpace(name))] = false
		}
	}
	var entries []Entry
	seen := map[string]bool{}
	for _, group := range manifest.Modern {
		for _, distro := range group {
			image := strings.ToLower(distro.Name)
			if len(wanted) > 0 {
				if _, ok := wanted[image]; !ok {
					continue
				}
			}
			if distro.AMD64.URL == "" {
				continue // ARM64-only entries are outside this builder's scope.
			}
			entry := Entry{distro.Name, image, distro.AMD64.URL, distro.AMD64.SHA256}
			if err := entry.validate(); err != nil {
				return nil, fmt.Errorf("%s: %w", distro.Name, err)
			}
			if seen[image] {
				return nil, fmt.Errorf("duplicate image %q in manifest", image)
			}
			seen[image] = true
			if _, ok := wanted[image]; ok {
				wanted[image] = true
			}
			entries = append(entries, entry)
		}
	}
	for name, found := range wanted {
		if !found {
			return nil, fmt.Errorf("unknown AMD64 distribution %q", name)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("manifest contains no usable modern AMD64 distributions")
	}
	if len(entries) > 256 {
		return nil, fmt.Errorf("manifest exceeds GitHub's 256-job matrix limit")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Image < entries[j].Image })
	return entries, nil
}

func fetchEntries(ctx context.Context, client *http.Client, source, filter string) ([]Entry, error) {
	var entries []Entry
	err := retry(ctx, 3, func() error {
		response, err := get(ctx, client, source)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		entries, err = parseManifest(response.Body, filter)
		return err
	})
	return entries, err
}
