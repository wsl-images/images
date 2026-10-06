package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type dockerRunner func(context.Context, ...string) error

type buildOptions struct {
	Owner, Quay, Revision string
	Push                  bool
	Now                   time.Time
}

func runDocker(ctx context.Context, args ...string) error {
	log.Printf("docker %s", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", args[0], err)
	}
	return nil
}

func build(ctx context.Context, client *http.Client, entry Entry, options buildOptions, docker dockerRunner) error {
	if err := entry.validate(); err != nil {
		return err
	}
	if !imageComponent.MatchString(options.Owner) {
		return fmt.Errorf("invalid GHCR owner %q", options.Owner)
	}
	if options.Quay != "" {
		parts := strings.Split(options.Quay, "/")
		if len(parts) != 3 || parts[0] != "quay.io" || !imageComponent.MatchString(parts[1]) || !imageComponent.MatchString(parts[2]) {
			return fmt.Errorf("invalid Quay repository %q", options.Quay)
		}
	}
	if err := docker(ctx, "info", "--format", "{{.OSType}}"); err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "wsl-image-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	archive := filepath.Join(directory, "rootfs.wsl")
	log.Printf("Downloading %s from %s", entry.Name, entry.URL)
	if err := download(ctx, client, entry.URL, archive, entry.SHA256); err != nil {
		return err
	}
	rootfs := filepath.Join(directory, "rootfs.tar")
	if err := normalizeArchive(ctx, archive, rootfs); err != nil {
		return err
	}
	version, err := archiveVersion(rootfs)
	if err != nil {
		return err
	}
	image := "wsl-local/" + entry.Image + ":sha256-" + entry.SHA256
	args := []string{"import", "--platform=linux/amd64", "--change", `CMD ["/bin/sh"]`}
	labels := []string{
		"org.opencontainers.image.source=https://github.com/" + options.Owner + "/images",
		"org.opencontainers.image.title=" + entry.Name,
		"org.opencontainers.image.version=" + version,
		"org.opencontainers.image.created=" + options.Now.Format(time.RFC3339),
		"org.opencontainers.image.revision=" + options.Revision,
		"io.wsl-images.upstream.sha256=" + entry.SHA256,
		"io.wsl-images.upstream.url=" + entry.URL,
	}
	for _, label := range labels {
		args = append(args, "--change", "LABEL "+label)
	}
	args = append(args, rootfs, image)
	if err := docker(ctx, args...); err != nil {
		return err
	}
	if err := docker(ctx, "run", "--rm", "--network=none", "--entrypoint=/bin/sh", image,
		"-ec", "test -r /etc/os-release || test -r /usr/lib/os-release"); err != nil {
		return fmt.Errorf("%s smoke test: %w", entry.Name, err)
	}
	if !options.Push {
		log.Printf("Built and smoke-tested %s (publishing disabled)", image)
		return nil
	}
	tags := []string{version, options.Now.Format("2006-01-02-150405"), "sha256-" + entry.SHA256, "latest"}
	base := "ghcr.io/" + options.Owner + "/" + entry.Image
	if err := publish(ctx, docker, image, base, "", tags); err != nil {
		return err
	}
	if options.Quay != "" {
		if err := publish(ctx, docker, image, options.Quay, entry.Image+"-", tags); err != nil {
			return err
		}
	}
	log.Printf("Published %s:%s", base, version)
	return nil
}

func publish(ctx context.Context, docker dockerRunner, image, repository, prefix string, tags []string) error {
	seen := map[string]bool{}
	for _, tag := range tags {
		fullTag := prefix + tag
		if !dockerTag.MatchString(fullTag) {
			return fmt.Errorf("invalid registry tag %q", fullTag)
		}
		target := repository + ":" + fullTag
		if seen[target] {
			continue
		}
		seen[target] = true
		if err := docker(ctx, "tag", image, target); err != nil {
			return err
		}
		if err := retry(ctx, 3, func() error { return docker(ctx, "push", target) }); err != nil {
			return err
		}
	}
	return nil
}
