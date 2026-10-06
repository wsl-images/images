package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func retry(ctx context.Context, attempts int, operation func() error) error {
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = operation(); err == nil {
			return nil
		}
		if attempt+1 < attempts {
			log.Printf("Attempt %d/%d failed: %v; retrying", attempt+1, attempts, err)
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return err
}

func get(ctx context.Context, client *http.Client, source string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "wsl-images-builder/2")
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d", source, response.StatusCode)
	}
	return response, nil
}

func download(ctx context.Context, client *http.Client, source, destination, expected string) error {
	want, err := normalizeSHA(expected)
	if err != nil {
		return err
	}
	part := destination + ".part"
	defer os.Remove(part)
	return retry(ctx, 3, func() error {
		response, err := get(ctx, client, source)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		file, err := os.Create(part)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(file, hash), response.Body)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		actual := fmt.Sprintf("%x", hash.Sum(nil))
		if actual != want {
			return fmt.Errorf("SHA256 mismatch for %s: expected %s, got %s", source, want, actual)
		}
		return os.Rename(part, destination)
	})
}
