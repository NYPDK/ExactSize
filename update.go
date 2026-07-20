package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	exactSizeLatestReleaseAPI  = "https://api.github.com/repos/NYPDK/ExactSize/releases/latest"
	exactSizeLatestReleasePage = "https://github.com/NYPDK/ExactSize/releases/latest"
)

type UpdateInfo struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion"`
	UpdateAvailable bool   `json:"updateAvailable"`
	ReleaseURL      string `json:"releaseURL"`
}

type releaseVersion struct {
	major int
	minor int
	patch int
}

func parseReleaseVersion(value string) (releaseVersion, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	value = strings.TrimPrefix(value, "V")
	if core, _, found := strings.Cut(value, "+"); found {
		value = core
	}
	if core, _, found := strings.Cut(value, "-"); found {
		value = core
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return releaseVersion{}, fmt.Errorf("invalid release version %q", value)
	}
	values := make([]int, len(parts))
	for index, part := range parts {
		if part == "" {
			return releaseVersion{}, fmt.Errorf("invalid release version %q", value)
		}
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 {
			return releaseVersion{}, fmt.Errorf("invalid release version %q", value)
		}
		values[index] = parsed
	}
	return releaseVersion{major: values[0], minor: values[1], patch: values[2]}, nil
}

func isNewerRelease(current, latest string) (bool, error) {
	currentVersion, err := parseReleaseVersion(current)
	if err != nil {
		return false, err
	}
	latestVersion, err := parseReleaseVersion(latest)
	if err != nil {
		return false, err
	}
	currentParts := [...]int{currentVersion.major, currentVersion.minor, currentVersion.patch}
	latestParts := [...]int{latestVersion.major, latestVersion.minor, latestVersion.patch}
	for index := range currentParts {
		if latestParts[index] != currentParts[index] {
			return latestParts[index] > currentParts[index], nil
		}
	}
	return false, nil
}

func checkLatestRelease(ctx context.Context, client *http.Client, apiURL, currentVersion string) (UpdateInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return UpdateInfo{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "ExactSize/"+currentVersion)

	response, err := client.Do(request)
	if err != nil {
		return UpdateInfo{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return UpdateInfo{}, fmt.Errorf("GitHub release check returned %s", response.Status)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return UpdateInfo{}, fmt.Errorf("decode GitHub release: %w", err)
	}
	latestVersion := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(release.TagName), "v"), "V")
	available, err := isNewerRelease(currentVersion, latestVersion)
	if err != nil {
		return UpdateInfo{}, err
	}
	return UpdateInfo{
		CurrentVersion:  currentVersion,
		LatestVersion:   latestVersion,
		UpdateAvailable: available,
		ReleaseURL:      exactSizeLatestReleasePage,
	}, nil
}

func (a *App) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	info, err := checkLatestRelease(r.Context(), a.updateClient, a.releaseAPIURL, version)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not check GitHub for updates")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (a *App) handleUpdateOpen(w http.ResponseWriter, _ *http.Request) {
	if err := a.openURL(exactSizeLatestReleasePage); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func openExternalURL(rawURL string) error {
	if runtime.GOOS != "linux" {
		return errors.New("opening links is currently supported on Linux only")
	}
	command, err := exec.LookPath("xdg-open")
	if err != nil {
		return errors.New("xdg-open is not available")
	}
	return exec.Command(command, rawURL).Start()
}

func defaultUpdateClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}
