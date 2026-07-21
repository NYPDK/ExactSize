//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	errorAlreadyExists             = syscall.Errno(183)
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
	createNoWindow                 = 0x08000000
	mbOK                           = 0x00000000
	mbIconWarning                  = 0x00000030
	mbIconError                    = 0x00000010
	mbSetForeground                = 0x00010000
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	createMutexW       = kernel32.NewProc("CreateMutexW")
	openProcess        = kernel32.NewProc("OpenProcess")
	getExitCodeProcess = kernel32.NewProc("GetExitCodeProcess")
	user32             = syscall.NewLazyDLL("user32.dll")
	messageBoxW        = user32.NewProc("MessageBoxW")
)

type windowsInstanceGuard struct {
	handle syscall.Handle
}

func (guard *windowsInstanceGuard) Close() error {
	if guard.handle == 0 {
		return nil
	}
	err := syscall.CloseHandle(guard.handle)
	guard.handle = 0
	return err
}

func instanceGuardAddress() string {
	// Local\ scopes the mutex to the interactive Windows session. That lets
	// separate signed-in sessions run ExactSize independently.
	return `Local\io.exactsize.ExactSize`
}

func acquireInstanceGuard(name string) (io.Closer, error) {
	namePointer, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("encode single-instance guard name: %w", err)
	}
	handle, _, callErr := createMutexW.Call(0, 0, uintptr(unsafe.Pointer(namePointer)))
	if handle == 0 {
		return nil, fmt.Errorf("create single-instance guard: %w", callErr)
	}
	guard := &windowsInstanceGuard{handle: syscall.Handle(handle)}
	if errors.Is(callErr, errorAlreadyExists) {
		_ = guard.Close()
		return nil, errAlreadyRunning
	}
	return guard, nil
}

func processIsRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, _, callErr := openProcess.Call(processQueryLimitedInformation, 0, uintptr(uint32(pid)))
	if handle == 0 {
		// Access denied still means the PID exists; other failures mean it is
		// safe to regard the browser profile as stale.
		return errors.Is(callErr, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(syscall.Handle(handle))
	var exitCode uint32
	ok, _, _ := getExitCodeProcess.Call(handle, uintptr(unsafe.Pointer(&exitCode)))
	return ok != 0 && exitCode == stillActive
}

func launchAppWindow(url string) (*exec.Cmd, bool, func(), error) {
	profileDir, cleanupProfile, err := createBrowserProfile()
	if err != nil {
		return nil, false, func() {}, fmt.Errorf("create temporary browser profile: %w", err)
	}
	seed := []byte(`{"brave":{"p3a":{"enabled":false,"notice_acknowledged":true},"stats_reporting":{"enabled":false}}}`)
	_ = os.WriteFile(filepath.Join(profileDir, "Local State"), seed, 0o600)
	chromeArgs := []string{
		"--app=" + url,
		"--new-window",
		"--no-first-run",
		"--disable-session-crashed-bubble",
		fmt.Sprintf("--window-size=%d,%d", minimumWindowWidth, minimumWindowHeight),
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-sync",
		"--disk-cache-size=1048576",
		"--media-cache-size=1048576",
		"--user-data-dir=" + profileDir,
	}

	var candidates []string
	if preferred := strings.TrimSpace(os.Getenv("EXACTSIZE_BROWSER")); preferred != "" {
		candidates = append(candidates, preferred)
	}
	for _, root := range []string{
		os.Getenv("ProgramW6432"),
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("LOCALAPPDATA"),
	} {
		if strings.TrimSpace(root) == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(root, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
		)
	}
	candidates = append(candidates, "msedge.exe", "chrome.exe", "brave.exe", "chromium.exe")

	seen := make(map[string]bool)
	for _, candidate := range candidates {
		path, err := resolveWindowsCommand(candidate)
		if err != nil || seen[strings.ToLower(path)] {
			continue
		}
		seen[strings.ToLower(path)] = true
		cmd := exec.Command(path, chromeArgs...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		if err := cmd.Start(); err == nil {
			return cmd, true, cleanupProfile, nil
		}
	}

	// The browser file input remains a safe input fallback when no Chromium
	// app-mode browser is installed, so open the UI in the registered browser.
	launcher, err := exec.LookPath("rundll32.exe")
	if err == nil {
		cmd := exec.Command(launcher, "url.dll,FileProtocolHandler", url)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
		if err := cmd.Start(); err == nil {
			cleanupProfile()
			return cmd, false, func() {}, nil
		}
	}

	cleanupProfile()
	return nil, false, func() {}, errors.New("no Windows browser could be opened")
}

func resolveWindowsCommand(candidate string) (string, error) {
	if filepath.IsAbs(candidate) || strings.ContainsAny(candidate, `/\`) {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return filepath.Clean(candidate), nil
		}
		return "", os.ErrNotExist
	}
	return exec.LookPath(candidate)
}

func appImageOwnedByCurrentUser(fs.FileInfo) bool { return true }

func expectedReleaseAssetName(releaseVersion string) string {
	return "ExactSize-" + releaseVersion + "-windows-x86_64.zip"
}

func configureBackgroundCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func showWindowsMessageBox(message string, fatal bool) {
	text, textErr := syscall.UTF16PtrFromString(message)
	title, titleErr := syscall.UTF16PtrFromString("ExactSize")
	if textErr != nil || titleErr != nil {
		return
	}
	flags := uintptr(mbOK | mbSetForeground | mbIconWarning)
	if fatal {
		flags = mbOK | mbSetForeground | mbIconError
	}
	messageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), flags)
}
