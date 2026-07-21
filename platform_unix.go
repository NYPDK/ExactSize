//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// acquireInstanceGuard uses the abstract Unix-socket namespace, so the guard
// is released by the kernel when ExactSize exits and never leaves a lock file.
func instanceGuardAddress() string {
	return "\x00io.exactsize.ExactSize-" + strconv.Itoa(os.Getuid())
}

func acquireInstanceGuard(name string) (io.Closer, error) {
	address := &net.UnixAddr{Name: name, Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, errAlreadyRunning
		}
		return nil, fmt.Errorf("create single-instance guard: %w", err)
	}
	return listener, nil
}

func processIsRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

func launchAppWindow(url string) (*exec.Cmd, bool, func(), error) {
	type candidate struct {
		command string
		args    []string
		wait    bool
		profile bool
	}

	profileDir, cleanupProfile, err := createBrowserProfile()
	if err != nil {
		return nil, false, func() {}, fmt.Errorf("create temporary browser profile: %w", err)
	}
	// Pre-acknowledge Brave's analytics notice and disable its reporting; a
	// fresh profile would otherwise show the banner on every launch. Chrome
	// and Chromium ignore these keys.
	seed := []byte(`{"brave":{"p3a":{"enabled":false,"notice_acknowledged":true},"stats_reporting":{"enabled":false}}}`)
	_ = os.WriteFile(filepath.Join(profileDir, "Local State"), seed, 0o600)
	chromeArgs := []string{
		"--app=" + url,
		"--new-window",
		"--no-first-run",
		"--disable-session-crashed-bubble",
		"--class=ExactSize",
		fmt.Sprintf("--window-size=%d,%d", minimumWindowWidth, minimumWindowHeight),
		"--ozone-platform=x11",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-sync",
		"--disk-cache-size=1048576",
		"--media-cache-size=1048576",
		"--user-data-dir=" + profileDir,
	}

	var candidates []candidate
	if preferred := strings.TrimSpace(os.Getenv("EXACTSIZE_BROWSER")); preferred != "" {
		candidates = append(candidates, candidate{preferred, chromeArgs, true, true})
	}
	for _, browser := range []string{
		"brave-browser", "brave", "google-chrome-stable", "google-chrome",
		"chromium", "chromium-browser", "microsoft-edge-stable", "microsoft-edge",
	} {
		candidates = append(candidates, candidate{browser, chromeArgs, true, true})
	}
	if _, err := exec.LookPath("flatpak"); err == nil {
		for _, appID := range []string{
			"com.brave.Browser", "org.chromium.Chromium", "com.google.Chrome",
		} {
			if exec.Command("flatpak", "info", appID).Run() != nil {
				continue
			}
			args := append([]string{"run", appID}, chromeArgs...)
			candidates = append(candidates, candidate{"flatpak", args, true, true})
		}
	}
	candidates = append(candidates,
		candidate{"firefox", []string{"--new-window", url}, false, false},
		candidate{"xdg-open", []string{url}, false, false},
	)

	for _, item := range candidates {
		path, err := exec.LookPath(item.command)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, item.args...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err == nil {
			if !item.profile {
				cleanupProfile()
			}
			return cmd, item.wait, cleanupProfile, nil
		}
	}

	cleanupProfile()
	return nil, false, func() {}, errors.New("no supported browser was found (Brave, Chrome, Chromium, Firefox, or xdg-open)")
}

func appImageOwnedByCurrentUser(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Uid == uint32(os.Geteuid())
}

func expectedReleaseAssetName(releaseVersion string) string {
	return expectedAppImageName(releaseVersion)
}

func configureBackgroundCommand(*exec.Cmd) {}

func showWindowsMessageBox(string, bool) {}
