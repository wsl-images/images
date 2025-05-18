package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// UrlInfo represents the URL and its checksum.
type UrlInfo struct {
	Url    string `json:"Url"`
	Sha256 string `json:"Sha256"`
}

// DistroDetail represents each distribution entry.
type DistroDetail struct {
	Name         string   `json:"Name"`
	FriendlyName string   `json:"FriendlyName"`
	Default      bool     `json:"Default"`
	Amd64Url     UrlInfo  `json:"Amd64Url"`
	Arm64Url     *UrlInfo `json:"Arm64Url,omitempty"`
}

// ModernDists represents the top-level JSON structure.
type ModernDists struct {
	ModernDistributions map[string][]DistroDetail `json:"ModernDistributions"`
	Default             string                    `json:"Default"`
	// The "Distributions" key is ignored for this workflow.
}

func main() {
	// Fetch distribution info from GitHub
	dists := fetchDistributionInfo()

	// Process all modern distribution groups
	for groupName, distroList := range dists.ModernDistributions {
		log.Printf("Processing distribution group: %s", groupName)

		// Process each distribution in the group
		for _, distro := range distroList {
			log.Printf("Building image for: %s (%s)", distro.Name, distro.FriendlyName)

			// Create a temporary file for the distribution tarball
			tarFilePath := fmt.Sprintf("%s.tar", distro.Name)

			// Download the distribution tarball (AMD64 only)
			downloadDistributionTarball(distro.Amd64Url.Url, tarFilePath)

			// Extract the version tag from the tarball
			tag := extractTagFromTarball(tarFilePath, distro.Amd64Url.Url)

			// With this line (using underscore to ignore unused return value):
			baseImageName, _, dateTag := importTarballToDocker(tarFilePath, tag, distro)

			// Push the Docker image to GitHub Packages
			pushDockerImage(baseImageName, tag, dateTag)

			// Clean up the tarball
			err := os.Remove(tarFilePath)
			if err != nil {
				log.Printf("Warning: Failed to clean up tarball: %v", err)
			}

			log.Printf("Completed building image for: %s", distro.Name)
		}
	}

	log.Printf("All distributions have been processed successfully")
}

// fetchDistributionInfo fetches and parses the distribution information JSON
func fetchDistributionInfo() ModernDists {
	jsonURL := "https://raw.githubusercontent.com/microsoft/WSL/master/distributions/DistributionInfo.json"
	resp, err := http.Get(jsonURL)
	if err != nil {
		log.Fatalf("Failed to fetch JSON: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read JSON response: %v", err)
	}

	var dists ModernDists
	if err := json.Unmarshal(body, &dists); err != nil {
		log.Fatalf("Failed to parse JSON: %v", err)
	}
	return dists
}

// downloadDistributionTarball downloads the tarball from the given URL
func downloadDistributionTarball(url string, filePath string) {
	log.Printf("Tarball URL: %s", url)
	if err := downloadFile(filePath, url); err != nil {
		log.Fatalf("Failed to download tarball: %v", err)
	}
	log.Printf("Downloaded tarball to %s", filePath)
}

// extractTagFromTarball extracts the version tag from the tarball
func extractTagFromTarball(tarFilePath string, url string) string {
	// First try direct tar extraction
	tag, err := extractTagFromTar(tarFilePath)
	if err != nil {
		log.Printf("Could not extract tag from tarball: %v", err)

		// Fall back to extracting version from URL
		log.Printf("Attempting to extract version from URL: %s", url)
		tag = extractVersionFromURL(url)
		if tag != "" {
			log.Printf("Extracted tag from URL: %s", tag)
			return tag
		}
		log.Fatalf("Failed to extract version information")
	}
	log.Printf("Extracted tag from os-release: %s", tag)
	return tag
}

// extractVersionFromURL extracts version information from the URL or filename
func extractVersionFromURL(url string) string {
	parts := strings.Split(url, "/")
	filename := parts[len(parts)-1]

	// Look for version pattern in filename (like 24.04.2)
	versionPattern := regexp.MustCompile(`(\d+\.\d+(\.\d+)?)`)
	matches := versionPattern.FindStringSubmatch(filename)
	if len(matches) > 0 {
		return matches[1]
	}
	return ""
}

// importTarballToDocker imports the tarball into Docker
func importTarballToDocker(tarFilePath string, tag string, distro DistroDetail) (string, string, string) {
	// Base image name without tag
	baseImageName := strings.ToLower(distro.Name)

	// Image name with version tag
	imageNameWithTag := baseImageName + ":" + tag

	// Import the image with the version tag
	importCmd := exec.Command("docker", "import", tarFilePath, imageNameWithTag)
	importCmd.Stdout = os.Stdout
	importCmd.Stderr = os.Stderr
	if err := importCmd.Run(); err != nil {
		log.Fatalf("Failed to import docker image: %v", err)
	}
	log.Printf("Docker image imported with tag %s", imageNameWithTag)

	// Tag the image as latest
	latestImageName := baseImageName + ":latest"
	tagLatestCmd := exec.Command("docker", "tag", imageNameWithTag, latestImageName)
	if err := tagLatestCmd.Run(); err != nil {
		log.Printf("Warning: Failed to tag image as latest: %v", err)
	} else {
		log.Printf("Image tagged as %s", latestImageName)
	}

	// Tag with today's date and time
	currentTime := time.Now().Format("2006-01-02-150405")
	dateImageName := baseImageName + ":" + currentTime
	tagDateCmd := exec.Command("docker", "tag", imageNameWithTag, dateImageName)
	if err := tagDateCmd.Run(); err != nil {
		log.Printf("Warning: Failed to tag image with date: %v", err)
	} else {
		log.Printf("Image tagged as %s", dateImageName)
	}

	return baseImageName, imageNameWithTag, currentTime
}

func pushDockerImage(baseImageName string, tag string, dateTag string) {
	// Get GitHub username from environment (set by GitHub Actions)
	githubUsername := os.Getenv("GITHUB_REPOSITORY_OWNER")
	if githubUsername == "" {
		// Fallback to local user if not in GitHub Actions
		githubUsername = "wsl-images"
	}

	// Make sure username is lowercase for GitHub Container Registry
	githubUsername = strings.ToLower(githubUsername)

	// Format for GitHub container registry
	repoName := strings.ToLower(baseImageName)
	ghcrBase := fmt.Sprintf("ghcr.io/%s/%s", githubUsername, repoName)

	// Format for Quay.io repository - without tag
	quayRepo := "quay.io/wsl-images/images"

	// Tag images for the GitHub container registry
	imageNameWithTag := baseImageName + ":" + tag
	ghcrImageTag := ghcrBase + ":" + tag
	ghcrLatestTag := ghcrBase + ":latest"
	ghcrDateTag := ghcrBase + ":" + dateTag

	// Tag with GitHub container registry URL
	log.Printf("Tagging %s as %s", imageNameWithTag, ghcrImageTag)
	tagCmd := exec.Command("docker", "tag", imageNameWithTag, ghcrImageTag)
	tagCmd.Stderr = os.Stderr
	if err := tagCmd.Run(); err != nil {
		log.Fatalf("Failed to tag image for GitHub Packages: %v", err)
	}

	// Tag latest for GitHub
	log.Printf("Tagging %s as %s", imageNameWithTag, ghcrLatestTag)
	tagLatestCmd := exec.Command("docker", "tag", imageNameWithTag, ghcrLatestTag)
	tagLatestCmd.Stderr = os.Stderr
	if err := tagLatestCmd.Run(); err != nil {
		log.Fatalf("Failed to tag latest image for GitHub Packages: %v", err)
	}

	// Tag with date for GitHub
	log.Printf("Tagging %s as %s", imageNameWithTag, ghcrDateTag)
	tagDateCmd := exec.Command("docker", "tag", imageNameWithTag, ghcrDateTag)
	tagDateCmd.Stderr = os.Stderr
	if err := tagDateCmd.Run(); err != nil {
		log.Fatalf("Failed to tag dated image for GitHub Packages: %v", err)
	}

	// Push all GitHub tags
	log.Printf("Pushing image %s to GitHub Packages", ghcrBase)
	pushCmd := exec.Command("docker", "push", "--all-tags", ghcrBase)
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		log.Fatalf("Failed to push docker images to GitHub: %v", err)
	}
	log.Printf("Docker images pushed successfully to GitHub Packages")

	// Tag images for Quay.io with proper tags that include the distribution name
	quayImageTag := fmt.Sprintf("%s:%s-%s", quayRepo, repoName, tag)
	quayLatestTag := fmt.Sprintf("%s:%s-latest", quayRepo, repoName)
	quayDateTag := fmt.Sprintf("%s:%s-%s", quayRepo, repoName, dateTag)

	// Tag with Quay.io repository URL
	log.Printf("Tagging %s as %s", imageNameWithTag, quayImageTag)
	tagQuayCmd := exec.Command("docker", "tag", imageNameWithTag, quayImageTag)
	tagQuayCmd.Stderr = os.Stderr
	if err := tagQuayCmd.Run(); err != nil {
		log.Fatalf("Failed to tag image for Quay.io: %v", err)
	}

	// Tag latest for Quay.io
	log.Printf("Tagging %s as %s", imageNameWithTag, quayLatestTag)
	tagQuayLatestCmd := exec.Command("docker", "tag", imageNameWithTag, quayLatestTag)
	tagQuayLatestCmd.Stderr = os.Stderr
	if err := tagQuayLatestCmd.Run(); err != nil {
		log.Fatalf("Failed to tag latest image for Quay.io: %v", err)
	}

	// Tag with date for Quay.io
	log.Printf("Tagging %s as %s", imageNameWithTag, quayDateTag)
	tagQuayDateCmd := exec.Command("docker", "tag", imageNameWithTag, quayDateTag)
	tagQuayDateCmd.Stderr = os.Stderr
	if err := tagQuayDateCmd.Run(); err != nil {
		log.Fatalf("Failed to tag dated image for Quay.io: %v", err)
	}

	// Push each Quay.io tag individually since we can't use --all-tags
	log.Printf("Pushing image tags to Quay.io")

	for _, tag := range []string{quayImageTag, quayLatestTag, quayDateTag} {
		pushQuayTagCmd := exec.Command("docker", "push", tag)
		pushQuayTagCmd.Stdout = os.Stdout
		pushQuayTagCmd.Stderr = os.Stderr
		if err := pushQuayTagCmd.Run(); err != nil {
			log.Fatalf("Failed to push docker image to Quay.io: %v", err)
		}
	}

	log.Printf("Docker images pushed successfully to Quay.io")
}

// downloadFile downloads a file from the given URL and saves it to the specified filepath
func downloadFile(filepath string, url string) error {
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// extractTagFromTar extracts just the os-release file from the tarball
func extractTagFromTar(tarPath string) (string, error) {
	// Create a temporary directory for extraction
	tempDir, err := os.MkdirTemp("", "wsl-extract-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir) // Clean up when done

	// Try to extract os-release files - run them separately to avoid failing if one pattern doesn't match
	extractPatterns := []string{"*etc/os-release", "*usr/lib/os-release"}

	for _, pattern := range extractPatterns {
		cmd := exec.Command("tar", "xf", tarPath, "-C", tempDir,
			"--no-same-owner", "--wildcards", pattern)

		// We intentionally ignore errors here as one pattern might not exist
		cmd.Run()
	}

	// Find any os-release file in the temp directory
	var stdout bytes.Buffer
	findCmd := exec.Command("find", tempDir, "-name", "os-release")
	findCmd.Stdout = &stdout
	if findCmd.Run() == nil && stdout.Len() > 0 {
		for _, path := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
			if content, err := os.ReadFile(path); err == nil {
				tag := parseOsRelease(string(content))
				if tag != "" {
					return tag, nil
				}
			}
		}
	}

	// Check specific paths as fallback
	osReleaseFiles := []string{
		"etc/os-release",
		"usr/lib/os-release",
	}

	for _, relPath := range osReleaseFiles {
		osReleasePath := filepath.Join(tempDir, relPath)
		if content, err := os.ReadFile(osReleasePath); err == nil {
			tag := parseOsRelease(string(content))
			if tag != "" {
				return tag, nil
			}
		}
	}

	return "", fmt.Errorf("os-release file not found after extraction")
}

// parseOsRelease looks for a VERSION_ID entry in the os-release file contents
func parseOsRelease(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "VERSION_ID=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return strings.Trim(parts[1], "\"")
			}
		}
	}
	return ""
}
