package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
)

// The .wsl suffix does not identify compression. Normalize before importing.
func normalizeArchive(ctx context.Context, source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	buffer := bufio.NewReader(input)
	magic, _ := buffer.Peek(6)
	var reader io.Reader = buffer
	var command *exec.Cmd
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(buffer)
		if err != nil {
			return err
		}
		defer gz.Close()
		reader = gz
	case bytes.HasPrefix(magic, []byte("BZh")):
		reader = bzip2.NewReader(buffer)
	case bytes.HasPrefix(magic, []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		command = exec.CommandContext(ctx, "xz", "--decompress", "--stdout", "--", source)
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		command = exec.CommandContext(ctx, "zstd", "--decompress", "--stdout", "--", source)
	}
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	if command != nil {
		command.Stdout, command.Stderr = output, os.Stderr
		err = command.Run()
	} else {
		_, err = io.Copy(output, reader)
	}
	closeErr := output.Close()
	if err != nil {
		return fmt.Errorf("decompress rootfs: %w", err)
	}
	return closeErr
}

// Read regular files directly; never extract or follow archive symlinks on the
// host. /etc/os-release is often a symlink to /usr/lib/os-release.
func archiveVersion(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	reader := tar.NewReader(file)
	releases := map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("invalid rootfs tar: %w", err)
		}
		name := path.Clean(header.Name)
		if (name == "etc/os-release" || name == "usr/lib/os-release") && header.Typeflag == tar.TypeReg {
			if header.Size > 64<<10 {
				return "", fmt.Errorf("oversized os-release file")
			}
			content, err := io.ReadAll(reader)
			if err != nil {
				return "", err
			}
			releases[name] = string(content)
		}
	}
	for _, name := range []string{"etc/os-release", "usr/lib/os-release"} {
		if content, exists := releases[name]; exists {
			return parseVersion(content), nil
		}
	}
	return "", fmt.Errorf("rootfs has no regular etc/os-release or usr/lib/os-release")
}

func parseVersion(content string) string {
	values := map[string]string{}
	for line := range strings.SplitSeq(content, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			values[key] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	for _, key := range []string{"VERSION_ID", "BUILD_ID"} {
		if dockerTag.MatchString(values[key]) {
			return values[key]
		}
	}
	// Rolling releases such as Arch have no VERSION_ID. URL numbers can be an
	// architecture or launcher version, so do not guess a version from the URL.
	return "rolling"
}
