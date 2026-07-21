package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	exactSizeLatestReleaseAPI  = "https://api.github.com/repos/NYPDK/ExactSize/releases/latest"
	exactSizeLatestReleasePage = "https://github.com/NYPDK/ExactSize/releases/latest"
	maxUpdateAssetSize         = int64(2 << 30)
)

type UpdateInfo struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion"`
	UpdateAvailable bool   `json:"updateAvailable"`
	ReleaseURL      string `json:"releaseURL"`
	AssetURL        string `json:"assetURL,omitempty"`
	AssetName       string `json:"assetName,omitempty"`
	AssetSize       int64  `json:"assetSize,omitempty"`
	AssetDigest     string `json:"assetDigest,omitempty"`
	CanSelfUpdate   bool   `json:"canSelfUpdate"`
	InstallReason   string `json:"installReason,omitempty"`
	tagName         string
}

type UpdateStatus struct {
	State           string `json:"state"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	AssetName       string `json:"assetName,omitempty"`
	DownloadedBytes int64  `json:"downloadedBytes,omitempty"`
	TotalBytes      int64  `json:"totalBytes,omitempty"`
	Message         string `json:"message,omitempty"`
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
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return releaseVersion{}, fmt.Errorf("invalid release version %q", value)
	}
	values := make([]int, len(parts))
	for index, part := range parts {
		if part == "" {
			return releaseVersion{}, fmt.Errorf("invalid release version %q", value)
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return releaseVersion{}, fmt.Errorf("invalid release version %q", value)
			}
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

func expectedAppImageName(releaseVersion string) string {
	return "ExactSize-" + releaseVersion + "-x86_64.AppImage"
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
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name               string `json:"name"`
			State              string `json:"state"`
			Size               int64  `json:"size"`
			Digest             string `json:"digest"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&release); err != nil {
		return UpdateInfo{}, fmt.Errorf("decode GitHub release: %w", err)
	}
	latestVersion := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(release.TagName), "v"), "V")
	available, err := isNewerRelease(currentVersion, latestVersion)
	if err != nil {
		return UpdateInfo{}, err
	}
	releaseURL := strings.TrimSpace(release.HTMLURL)
	if releaseURL == "" {
		releaseURL = exactSizeLatestReleasePage
	}
	info := UpdateInfo{
		CurrentVersion:  currentVersion,
		LatestVersion:   latestVersion,
		UpdateAvailable: available,
		ReleaseURL:      releaseURL,
		tagName:         strings.TrimSpace(release.TagName),
	}
	if !available {
		return info, nil
	}
	expectedName := expectedAppImageName(latestVersion)
	for _, asset := range release.Assets {
		if asset.Name != expectedName || asset.State != "uploaded" {
			continue
		}
		if info.AssetName != "" {
			return UpdateInfo{}, fmt.Errorf("release contains duplicate %s assets", expectedName)
		}
		info.AssetName = asset.Name
		info.AssetURL = asset.BrowserDownloadURL
		info.AssetSize = asset.Size
		info.AssetDigest = strings.ToLower(strings.TrimSpace(asset.Digest))
	}
	return info, nil
}

func (a *App) checkedUpdate(ctx context.Context) (UpdateInfo, error) {
	info, err := checkLatestRelease(ctx, a.updateClient, a.releaseAPIURL, version)
	if err != nil {
		return UpdateInfo{}, err
	}
	if !info.UpdateAvailable {
		return info, nil
	}
	if err := validateUpdateAsset(info); err != nil {
		info.InstallReason = err.Error()
		return info, nil
	}
	if _, err := a.appImagePath(); err != nil {
		info.InstallReason = err.Error()
		return info, nil
	}
	info.CanSelfUpdate = true
	return info, nil
}

func validateUpdateAsset(info UpdateInfo) error {
	if info.AssetName != expectedAppImageName(info.LatestVersion) {
		return errors.New("the release does not include the expected x86_64 AppImage")
	}
	if info.AssetSize <= 0 || info.AssetSize > maxUpdateAssetSize {
		return errors.New("the release AppImage has an invalid size")
	}
	digest := strings.TrimPrefix(info.AssetDigest, "sha256:")
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || info.AssetDigest != "sha256:"+digest {
		return errors.New("the release AppImage does not have a valid SHA-256 digest")
	}
	assetURL, err := url.Parse(info.AssetURL)
	if err != nil || assetURL.Scheme != "https" || assetURL.Hostname() != "github.com" || assetURL.RawQuery != "" || assetURL.Fragment != "" {
		return errors.New("the release AppImage has an untrusted download URL")
	}
	wantPath := "/NYPDK/ExactSize/releases/download/" + info.tagName + "/" + info.AssetName
	if assetURL.Path != wantPath {
		return errors.New("the release AppImage URL does not match the release tag")
	}
	return nil
}

func (a *App) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	info, err := a.checkedUpdate(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not check GitHub for updates")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (a *App) handleUpdateOpenAsset(w http.ResponseWriter, r *http.Request) {
	info, err := a.checkedUpdate(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not check GitHub for updates")
		return
	}
	if !info.UpdateAvailable {
		writeError(w, http.StatusConflict, "ExactSize is already up to date")
		return
	}
	if err := validateUpdateAsset(info); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := a.openURL(info.AssetURL); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	info, err := a.checkedUpdate(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not check GitHub for updates")
		return
	}
	if !info.UpdateAvailable {
		writeError(w, http.StatusConflict, "ExactSize is already up to date")
		return
	}
	if !info.CanSelfUpdate {
		writeError(w, http.StatusConflict, info.InstallReason)
		return
	}
	appImagePath, err := a.appImagePath()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	a.mu.Lock()
	if a.job != nil && !a.job.isTerminal() {
		a.mu.Unlock()
		writeError(w, http.StatusConflict, "wait for the current encode to finish before updating")
		return
	}
	if a.updating {
		a.mu.Unlock()
		writeError(w, http.StatusConflict, "an ExactSize update is already downloading")
		return
	}
	a.updating = true
	a.updateMu.Lock()
	a.updateStatus = UpdateStatus{
		State:         "downloading",
		LatestVersion: info.LatestVersion,
		AssetName:     info.AssetName,
		TotalBytes:    info.AssetSize,
		Message:       "Downloading the verified AppImage…",
	}
	updateContext, updateCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	a.updateCancel = updateCancel
	a.updateDone = make(chan struct{})
	status := a.updateStatus
	a.updateMu.Unlock()
	a.mu.Unlock()

	go a.installUpdate(updateContext, updateCancel, info, appImagePath)
	writeJSON(w, http.StatusAccepted, status)
}

func (a *App) handleUpdateStatus(w http.ResponseWriter, _ *http.Request) {
	a.updateMu.RLock()
	status := a.updateStatus
	a.updateMu.RUnlock()
	if status.State == "" {
		status.State = "idle"
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *App) installUpdate(ctx context.Context, cancel context.CancelFunc, info UpdateInfo, appImagePath string) {
	defer cancel()
	err := downloadAndReplaceAppImage(ctx, a.updateDownloadClient, appImagePath, info, func(downloaded int64) {
		a.updateMu.Lock()
		a.updateStatus.DownloadedBytes = downloaded
		a.updateMu.Unlock()
	})

	a.mu.Lock()
	a.updateMu.Lock()
	if err != nil {
		a.updateStatus.State = "failed"
		a.updateStatus.Message = err.Error()
	} else {
		a.updateStatus.State = "installed"
		a.updateStatus.DownloadedBytes = info.AssetSize
		a.updateStatus.Message = "Update installed. Close and reopen ExactSize to use it."
	}
	a.updating = false
	done := a.updateDone
	a.updateCancel = nil
	a.updateDone = nil
	a.updateMu.Unlock()
	a.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func runningAppImagePath() (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("automatic installation is available for Linux AppImages only")
	}
	if strings.TrimSpace(os.Getenv("APPDIR")) == "" {
		return "", errors.New("automatic installation is available when running the AppImage build")
	}
	rawPath := strings.TrimSpace(os.Getenv("APPIMAGE"))
	if rawPath == "" {
		return "", errors.New("automatic installation is available when running the AppImage build")
	}
	resolved, err := filepath.EvalSymlinks(rawPath)
	if err != nil {
		return "", fmt.Errorf("resolve the running AppImage: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve the running AppImage: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect the running AppImage: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("the running AppImage path is not a regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("the running AppImage is not owned by the current user")
	}
	if err := validateAppImageMagic(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func downloadAndReplaceAppImage(ctx context.Context, client *http.Client, currentPath string, asset UpdateInfo, progress func(int64)) error {
	currentInfo, err := os.Lstat(currentPath)
	if err != nil {
		return fmt.Errorf("inspect the installed AppImage: %w", err)
	}
	if !currentInfo.Mode().IsRegular() {
		return errors.New("the installed AppImage is not a regular file")
	}
	directory := filepath.Dir(currentPath)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(currentPath)+".update-*")
	if err != nil {
		return fmt.Errorf("create the update beside the AppImage: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.AssetURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "ExactSize/"+version)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download the AppImage: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("AppImage download returned %s", response.Status)
	}

	hash := sha256.New()
	written, err := copyUpdateWithProgress(temporary, io.LimitReader(response.Body, asset.AssetSize+1), hash, progress)
	if err != nil {
		return fmt.Errorf("write the AppImage update: %w", err)
	}
	if written != asset.AssetSize {
		return fmt.Errorf("AppImage download size was %d bytes, expected %d", written, asset.AssetSize)
	}
	wantDigest, err := hex.DecodeString(strings.TrimPrefix(asset.AssetDigest, "sha256:"))
	if err != nil || len(wantDigest) != sha256.Size || !equalBytes(hash.Sum(nil), wantDigest) {
		return errors.New("downloaded AppImage failed SHA-256 verification")
	}
	mode := (currentInfo.Mode().Perm() & 0o755) | 0o100
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("make the AppImage executable: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush the AppImage update: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close the AppImage update: %w", err)
	}
	closed = true
	if err := validateAppImageMagic(temporaryPath); err != nil {
		return fmt.Errorf("validate the downloaded AppImage: %w", err)
	}
	latestInfo, err := os.Lstat(currentPath)
	if err != nil {
		return fmt.Errorf("recheck the installed AppImage: %w", err)
	}
	if !os.SameFile(currentInfo, latestInfo) {
		return errors.New("the installed AppImage changed during the update; nothing was replaced")
	}
	if err := os.Rename(temporaryPath, currentPath); err != nil {
		return fmt.Errorf("atomically replace the AppImage: %w", err)
	}
	if directoryHandle, err := os.Open(directory); err == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

func copyUpdateWithProgress(destination io.Writer, source io.Reader, hash io.Writer, progress func(int64)) (int64, error) {
	buffer := make([]byte, 256<<10)
	var written int64
	for {
		count, readErr := source.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			if _, err := hash.Write(chunk); err != nil {
				return written, err
			}
			outputCount, err := destination.Write(chunk)
			written += int64(outputCount)
			if progress != nil {
				progress(written)
			}
			if err != nil {
				return written, err
			}
			if outputCount != count {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

func validateAppImageMagic(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open AppImage: %w", err)
	}
	defer file.Close()
	header := make([]byte, 11)
	if _, err := io.ReadFull(file, header); err != nil {
		return errors.New("file is too small to be an AppImage")
	}
	if string(header[:4]) != "\x7fELF" || string(header[8:11]) != "AI\x02" {
		return errors.New("file is not an AppImage type-2 executable")
	}
	return nil
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
	return &http.Client{Timeout: 8 * time.Second}
}

func defaultUpdateDownloadClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" {
				return errors.New("refusing a non-HTTPS update redirect")
			}
			if len(via) >= 10 {
				return errors.New("too many update redirects")
			}
			return nil
		},
	}
}
