package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func checksum(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func manifestFixture(distributions ...Distribution) string {
	data, _ := json.Marshal(map[string]any{"ModernDistributions": map[string]any{"test": distributions}})
	return string(data)
}

func TestManifest(t *testing.T) {
	valid := Distribution{"Ubuntu", URLInfo{"https://example.com/rootfs.wsl", "0x" + strings.Repeat("AB", 32)}}
	other := Distribution{"archlinux", URLInfo{"https://example.com/arch.wsl", strings.Repeat("cd", 32)}}
	entries, err := parseManifest(strings.NewReader(manifestFixture(valid, other)), "")
	if err != nil || len(entries) != 2 || entries[0].Name != "archlinux" || entries[1].SHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("entries=%+v, error=%v", entries, err)
	}
	entries, err = parseManifest(strings.NewReader(manifestFixture(valid, other)), " UBUNTU ")
	if err != nil || len(entries) != 1 || entries[0].Name != "Ubuntu" {
		t.Fatalf("filter: entries=%+v, error=%v", entries, err)
	}
	for name, input := range map[string]string{
		"missing modern key": `{ "Distributions": [] }`,
		"empty":              manifestFixture(),
		"duplicate":          manifestFixture(valid, valid),
		"bad json":           `<html>upstream error</html>`,
		"bad checksum":       manifestFixture(Distribution{"Ubuntu", URLInfo{"https://example.com/rootfs", "abc"}}),
		"unsafe name":        manifestFixture(Distribution{"../../tmp/pwned", valid.AMD64}),
		"insecure url":       manifestFixture(Distribution{"Ubuntu", URLInfo{"http://example.com/rootfs", valid.AMD64.SHA256}}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseManifest(strings.NewReader(input), ""); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	if _, err := parseManifest(strings.NewReader(manifestFixture(valid)), "typo"); err == nil {
		t.Fatal("unknown distribution silently accepted")
	}
}

func TestDownload(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"success", "retry", "404", "bad checksum", "truncated"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			attempts := 0
			payload := []byte("verified rootfs contents")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if scenario == "404" {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				if scenario == "retry" && attempts == 1 {
					http.Error(w, "try again", http.StatusServiceUnavailable)
					return
				}
				if scenario == "truncated" {
					w.Header().Set("Content-Length", "999")
				}
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			want := checksum(payload)
			if scenario == "bad checksum" {
				want = strings.Repeat("0", 64)
			}
			destination := filepath.Join(t.TempDir(), "rootfs")
			err := download(context.Background(), server.Client(), server.URL, destination, want)
			if scenario == "success" || scenario == "retry" {
				if err != nil {
					t.Fatal(err)
				}
				got, _ := os.ReadFile(destination)
				if !bytes.Equal(got, payload) {
					t.Fatal("download contents differ")
				}
			} else if err == nil {
				t.Fatal("bad download accepted")
			} else if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
				t.Fatal("failed download left a final file")
			}
			if _, err := os.Stat(destination + ".part"); !os.IsNotExist(err) {
				t.Fatal("partial download not cleaned up")
			}
		})
	}
}

func tarFixture(t *testing.T, release, location string, symlink bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if symlink {
		if err := writer.WriteHeader(&tar.Header{Name: "etc/os-release", Typeflag: tar.TypeSymlink, Linkname: "../usr/lib/os-release"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.WriteHeader(&tar.Header{Name: location, Mode: 0644, Size: int64(len(release))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(release)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestArchiveCompressionAndSymlinks(t *testing.T) {
	raw := tarFixture(t, "ID=test\nVERSION_ID=\"24.04\"\n", "./usr/lib/os-release", true)
	for _, compression := range []string{"tar", "gzip", "bzip2", "xz", "zstd"} {
		t.Run(compression, func(t *testing.T) {
			payload := raw
			if compression == "gzip" {
				var buffer bytes.Buffer
				writer := gzip.NewWriter(&buffer)
				_, _ = writer.Write(raw)
				_ = writer.Close()
				payload = buffer.Bytes()
			} else if compression != "tar" {
				if _, err := exec.LookPath(compression); err != nil {
					t.Skipf("%s not installed (all formats exercised on Linux CI)", compression)
				}
				cmd := exec.Command(compression, "-c")
				cmd.Stdin = bytes.NewReader(raw)
				var err error
				payload, err = cmd.Output()
				if err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			source, dest := filepath.Join(dir, "source.wsl"), filepath.Join(dir, "rootfs.tar")
			if err := os.WriteFile(source, payload, 0600); err != nil {
				t.Fatal(err)
			}
			if err := normalizeArchive(context.Background(), source, dest); err != nil {
				t.Fatal(err)
			}
			version, err := archiveVersion(dest)
			if err != nil || version != "24.04" {
				t.Fatalf("version=%s error=%v", version, err)
			}
		})
	}
}

func TestArchiveRejectsInvalidAndTraversal(t *testing.T) {
	for _, payload := range [][]byte{[]byte("not a tarball"), tarFixture(t, "VERSION_ID=1", "../../etc/os-release", false)} {
		file := filepath.Join(t.TempDir(), "rootfs.tar")
		_ = os.WriteFile(file, payload, 0600)
		if _, err := archiveVersion(file); err == nil {
			t.Fatal("invalid rootfs accepted")
		}
	}
}

func TestVersion(t *testing.T) {
	for input, want := range map[string]string{
		"VERSION_ID=\"24.04\"\n":         "24.04",
		"VERSION_ID='13'\r\n":            "13",
		"ID=arch\nBUILD_ID=20261001\n":   "20261001",
		"ID=arch\n":                      "rolling",
		"VERSION_ID=bad/tag\n":           "rolling",
		"BUILD_ID=99\nVERSION_ID=16.0\n": "16.0",
	} {
		if got := parseVersion(input); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBuildPublishingBoundaries(t *testing.T) {
	for _, scenario := range []string{"local", "publish", "smoke failure", "checksum failure", "quay failure"} {
		t.Run(scenario, func(t *testing.T) {
			payload := tarFixture(t, "VERSION_ID=24.04\n", "etc/os-release", false)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
			defer server.Close()
			entry := Entry{"Ubuntu", "ubuntu", server.URL, checksum(payload)}
			if scenario == "checksum failure" {
				entry.SHA256 = strings.Repeat("0", 64)
			}
			var calls [][]string
			docker := func(_ context.Context, args ...string) error {
				calls = append(calls, append([]string(nil), args...))
				if (scenario == "smoke failure" && args[0] == "run") || (scenario == "quay failure" && args[0] == "push" && strings.HasPrefix(args[1], "quay.io/")) {
					return errors.New("simulated Docker failure")
				}
				return nil
			}
			err := build(context.Background(), server.Client(), entry, buildOptions{Owner: "wsl-images", Quay: "quay.io/wsl-images/images", Push: scenario != "local", Now: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}, docker)
			wantError := strings.Contains(scenario, "failure")
			if (err != nil) != wantError {
				t.Fatalf("error=%v, expected error=%v", err, wantError)
			}
			var pushed []string
			for _, args := range calls {
				if args[0] == "push" {
					pushed = append(pushed, args[1])
				}
			}
			if scenario == "local" || scenario == "smoke failure" || scenario == "checksum failure" {
				if len(pushed) != 0 {
					t.Fatalf("unexpected publication: %v", pushed)
				}
			}
			if scenario == "publish" && len(pushed) != 8 {
				t.Fatalf("wanted four tags per registry, got %v", pushed)
			}
			if scenario == "quay failure" && (len(pushed) < 4 || pushed[3] != "ghcr.io/wsl-images/ubuntu:latest") {
				t.Fatalf("mirror failure prevented GHCR publishing: %v", pushed)
			}
			for _, args := range calls {
				if args[0] == "import" {
					if _, err := os.Stat(args[len(args)-2]); !os.IsNotExist(err) {
						t.Fatal("temporary rootfs not cleaned up")
					}
				}
			}
		})
	}
}

func TestPublishOnlyRequestedTags(t *testing.T) {
	var pushed []string
	docker := func(_ context.Context, args ...string) error {
		if args[0] == "push" {
			pushed = append(pushed, args[1:]...)
		}
		return nil
	}
	err := publish(context.Background(), docker, "local:tag", "ghcr.io/test/arch", "", []string{"rolling", "latest", "latest"})
	if err != nil || !reflect.DeepEqual(pushed, []string{"ghcr.io/test/arch:rolling", "ghcr.io/test/arch:latest"}) {
		t.Fatalf("pushes=%v, error=%v", pushed, err)
	}
}

func TestRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := retry(ctx, 3, func() error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled operation ran: called=%v error=%v", called, err)
	}
}
