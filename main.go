package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "1.9.1"

var errAlreadyRunning = errors.New("ExactSize is already running")

const alreadyRunningMessage = "ExactSize is already running. Close the existing window before opening another one."

const (
	minimumWindowWidth   = 1040
	minimumWindowHeight  = 700
	browserProfilePrefix = "exactsize-browser-"
	browserProfileOwner  = ".exactsize-owner-pid"
)

//go:embed web/*
var webAssets embed.FS

//go:embed packaging/exactsize-256.png
var iconPNG []byte

func main() {
	if err := run(); err != nil {
		if errors.Is(err, errAlreadyRunning) {
			if runtime.GOOS == "linux" && os.Getenv("EXACTSIZE_HEADLESS") != "1" {
				showWarningDialog(alreadyRunningMessage)
			}
			return
		}
		log.Printf("ExactSize: %v", err)
		if runtime.GOOS == "linux" {
			showFatalDialog(err.Error())
		}
		os.Exit(1)
	}
}

func run() error {
	instanceGuard, err := acquireInstanceGuard(instanceGuardAddress())
	if err != nil {
		return err
	}
	defer instanceGuard.Close()

	ffmpeg, err := locateTool("EXACTSIZE_FFMPEG", "ffmpeg")
	if err != nil {
		return fmt.Errorf("FFmpeg was not found: %w", err)
	}
	ffprobe, err := locateTool("EXACTSIZE_FFPROBE", "ffprobe")
	if err != nil {
		return fmt.Errorf("ffprobe was not found: %w", err)
	}

	webRoot, err := fs.Sub(webAssets, "web")
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start local UI server: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token := randomToken()
	app := newApp(ffmpeg, ffprobe, token, webRoot)
	// Warm the FFmpeg + GPU capability probes while the browser starts.
	go func() { _, _ = app.inspectRuntime() }()
	integrateAppImage()
	server := &http.Server{
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	app.shutdown = func() { stop() }

	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	url := fmt.Sprintf("http://%s/?token=%s", listener.Addr().String(), token)
	if os.Getenv("EXACTSIZE_HEADLESS") == "1" {
		fmt.Println(url)
	} else {
		hideTitleBarOnKDE()
		browser, waitForWindow, cleanupBrowser, err := launchAppWindow(url)
		if err != nil {
			_ = listener.Close()
			return fmt.Errorf("open application window: %w", err)
		}
		defer cleanupBrowser()
		if waitForWindow {
			go func() {
				_ = browser.Wait()
				stop()
			}()
		}
	}

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	app.cancelCurrentJob()
	_ = server.Shutdown(shutdownCtx)
	return nil
}

// acquireInstanceGuard uses Linux's abstract Unix-socket namespace, so the
// guard is released by the kernel when ExactSize exits and never leaves a lock
// file behind. Scoping the name by user allows independent desktop sessions.
func instanceGuardAddress() string {
	return "\x00io.exactsize.ExactSize-" + strconv.Itoa(os.Getuid())
}

func acquireInstanceGuard(name string) (*net.UnixListener, error) {
	address := &net.UnixAddr{
		Name: name,
		Net:  "unix",
	}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, errAlreadyRunning
		}
		return nil, fmt.Errorf("create single-instance guard: %w", err)
	}
	return listener, nil
}

func locateTool(envName, name string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv(envName)); configured != "" {
		if info, err := os.Stat(configured); err == nil && !info.IsDir() {
			return configured, nil
		}
		return "", fmt.Errorf("%s points to an invalid file", envName)
	}

	if executable, err := os.Executable(); err == nil {
		dir := filepath.Dir(executable)
		for _, candidate := range []string{
			filepath.Join(dir, name),
			filepath.Join(dir, "..", "lib", "exactsize", name),
			filepath.Join(dir, "..", "bin", name),
		} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return filepath.Clean(candidate), nil
			}
		}
	}

	return exec.LookPath(name)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func randomToken() string {
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(data)
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

func cleanupStaleBrowserProfiles() {
	profiles, _ := filepath.Glob(filepath.Join(os.TempDir(), browserProfilePrefix+"*"))
	for _, profileDir := range profiles {
		owner, err := os.ReadFile(filepath.Join(profileDir, browserProfileOwner))
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(owner)))
			if parseErr == nil && processIsRunning(pid) {
				continue
			}
			_ = os.RemoveAll(profileDir)
			continue
		}

		// Profiles from older releases have no owner marker. Leave recent ones
		// alone in case an older ExactSize instance is still using one, but
		// remove abandoned legacy profiles on a later launch.
		if info, statErr := os.Stat(profileDir); statErr == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(profileDir)
		}
	}
}

func createBrowserProfile() (string, func(), error) {
	cleanupStaleBrowserProfiles()
	profileDir, err := os.MkdirTemp("", browserProfilePrefix)
	if err != nil {
		return "", func() {}, err
	}
	if err := os.WriteFile(
		filepath.Join(profileDir, browserProfileOwner),
		[]byte(strconv.Itoa(os.Getpid())),
		0o600,
	); err != nil {
		_ = os.RemoveAll(profileDir)
		return "", func() {}, err
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() { _ = os.RemoveAll(profileDir) })
	}
	return profileDir, cleanup, nil
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
		// XWayland instead of native Wayland: X11 windows can start a real
		// compositor move/resize (_NET_WM_MOVERESIZE), which native Wayland
		// offers no external API for, and --class works as the window class.
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

// integrateAppImage installs a launcher entry and icons under ~/.local/share
// when running from an AppImage. Launchers always point at a stable symlink
// which is atomically retargeted to the exact AppImage currently running.
func integrateAppImage() {
	appimage := strings.TrimSpace(os.Getenv("APPIMAGE"))
	if appimage == "" || runtime.GOOS != "linux" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	_ = installAppImageIntegration(appimage, home)
}

func installAppImageIntegration(appimage, home string) error {
	resolvedAppImage, err := filepath.EvalSymlinks(appimage)
	if err != nil {
		return fmt.Errorf("resolve AppImage path: %w", err)
	}
	resolvedAppImage, err = filepath.Abs(resolvedAppImage)
	if err != nil {
		return fmt.Errorf("make AppImage path absolute: %w", err)
	}
	if !fileExists(resolvedAppImage) {
		return fmt.Errorf("AppImage is not a regular file: %s", resolvedAppImage)
	}

	share := filepath.Join(home, ".local", "share")
	pngPath := filepath.Join(share, "icons", "hicolor", "256x256", "apps", "exactsize.png")
	svgPath := filepath.Join(share, "icons", "hicolor", "scalable", "apps", "exactsize.svg")
	desktopPath := filepath.Join(share, "applications", "io.exactsize.ExactSize.desktop")
	link := filepath.Join(share, "exactsize", "ExactSize.AppImage")
	for _, dir := range []string{filepath.Dir(pngPath), filepath.Dir(svgPath), filepath.Dir(desktopPath), filepath.Dir(link)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(pngPath, iconPNG, 0o644); err != nil {
		return err
	}
	if svg, err := fs.ReadFile(webAssets, "web/icon.svg"); err == nil {
		if err := os.WriteFile(svgPath, svg, 0o644); err != nil {
			return err
		}
	}

	temporaryLink := fmt.Sprintf("%s.new-%d", link, os.Getpid())
	_ = os.Remove(temporaryLink)
	if err := os.Symlink(resolvedAppImage, temporaryLink); err != nil {
		return fmt.Errorf("create current AppImage link: %w", err)
	}
	if err := os.Rename(temporaryLink, link); err != nil {
		_ = os.Remove(temporaryLink)
		return fmt.Errorf("activate current AppImage link: %w", err)
	}

	// Exec quoting per the desktop entry spec.
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(link)
	desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=ExactSize
GenericName=Video size compressor
Comment=Compress videos to a strict maximum file size
Exec="%s"
Icon=exactsize
Terminal=false
Categories=AudioVideo;Video;
Keywords=video;compress;encode;ffmpeg;av1;h264;h265;h266;vp9;
StartupNotify=true
StartupWMClass=ExactSize
X-AppImage-Version=%s
`, quoted, version)
	if err := writeFileIfChanged(desktopPath, []byte(desktop), 0o644); err != nil {
		return err
	}

	// Keep an existing desktop shortcut in sync too. We deliberately do not
	// create one: only shortcuts already identified as ExactSize are updated.
	desktopDir := xdgUserDir("DESKTOP", home)
	if desktopDir == "" {
		desktopDir = filepath.Join(home, "Desktop")
	}
	entries, _ := os.ReadDir(desktopDir)
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".desktop") {
			continue
		}
		path := filepath.Join(desktopDir, entry.Name())
		existing, err := os.ReadFile(path)
		if err != nil || !isExactSizeDesktopEntry(existing) {
			continue
		}
		if err := writeFileIfChanged(path, []byte(desktop), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func isExactSizeDesktopEntry(data []byte) bool {
	entry := string(data)
	return strings.Contains(entry, "[Desktop Entry]") &&
		(strings.Contains(entry, "StartupWMClass=ExactSize") || strings.Contains(entry, "Name=ExactSize\n"))
}

func writeFileIfChanged(path string, data []byte, mode fs.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return nil
	}
	return os.WriteFile(path, data, mode)
}

// exactSizeClassPattern matches the app window class: "ExactSize" under X11
// (where --class applies) and the URL-derived "brave-127.0.0.1__-Default"
// style under native Wayland browsers. Breeze evaluates title matches only at
// window creation, before the page title has loaded, so the class is the
// reliable anchor; the pattern stays unanchored because Breeze matches the
// combined "name class" string, and it must not start with a dash or
// kwriteconfig reads it as an option.
const exactSizeClassPattern = `(127\.0\.0\.1__-|[Ee]xact[Ss]ize)`

// legacyClassPatterns are patterns written by earlier releases; a matching
// Breeze exception is upgraded in place.
var legacyClassPatterns = []string{`127\.0\.0\.1__-`}

// hideTitleBarOnKDE hides the native title bar of the app window while
// keeping the window frame. It writes a Breeze decoration override, which
// preserves Plasma's rounded corners, shadows, and resize borders. Earlier
// releases forced a bare "no border" KWin rule instead; that rule is removed
// if present.
func hideTitleBarOnKDE() {
	if !strings.Contains(os.Getenv("XDG_CURRENT_DESKTOP"), "KDE") {
		return
	}
	suffix := "6"
	writer, err := exec.LookPath("kwriteconfig6")
	if err != nil {
		suffix = "5"
		if writer, err = exec.LookPath("kwriteconfig5"); err != nil {
			return
		}
	}
	write := func(file, group, key, value string) error {
		return exec.Command(writer, "--file", file, "--group", group, "--key", key, value).Run()
	}
	read := func(file, group, key string) string {
		reader, err := exec.LookPath("kreadconfig" + suffix)
		if err != nil {
			return ""
		}
		output, _ := exec.Command(reader, "--file", file, "--group", group, "--key", key).Output()
		return strings.TrimSpace(string(output))
	}

	changed := false

	// Migration: drop the 1.1.x "no titlebar and frame" rule, and make sure
	// the minimum-size rule is listed.
	rules := read("kwinrulesrc", "General", "rules")
	var kept []string
	hasMinSize := false
	for _, rule := range strings.Split(rules, ",") {
		trimmed := strings.TrimSpace(rule)
		if trimmed == "" || trimmed == "exactsize-noborder" {
			continue
		}
		if trimmed == "exactsize-minsize" {
			hasMinSize = true
		}
		kept = append(kept, trimmed)
	}
	if !hasMinSize {
		kept = append(kept, "exactsize-minsize")
	}
	// The layout needs the configured minimum; below it, scrollbars appear. A forced
	// KWin minimum covers every resize path, including window edges. Written
	// unconditionally so pattern updates reach existing installs.
	minSizeSettings := [][2]string{
		{"Description", "ExactSize app window: minimum size"},
		{"minsize", fmt.Sprintf("%d,%d", minimumWindowWidth, minimumWindowHeight)},
		{"minsizerule", "2"},
		{"title", "ExactSize"},
		{"titlematch", "1"},
		{"wmclass", exactSizeClassPattern},
		{"wmclassmatch", "3"}, // regex
		{"types", "1"},
	}
	for _, setting := range minSizeSettings {
		if read("kwinrulesrc", "exactsize-minsize", setting[0]) == setting[1] {
			continue
		}
		if err := write("kwinrulesrc", "exactsize-minsize", setting[0], setting[1]); err != nil {
			return
		}
		changed = true
	}
	if joined := strings.Join(kept, ","); joined != rules {
		if write("kwinrulesrc", "General", "rules", joined) == nil {
			changed = true
		}
	}

	// Install the Breeze override once. The config file is the source of
	// truth: look for our pattern among existing exception groups. KConfig
	// stores backslashes doubled, so normalize before comparing.
	home, _ := os.UserHomeDir()
	data, _ := os.ReadFile(filepath.Join(home, ".config", "breezerc"))
	normalized := strings.ReplaceAll(string(data), `\\`, `\`)
	// Upgrade an exception written by an earlier release in place.
	if !strings.Contains(normalized, exactSizeClassPattern) {
		currentGroup := ""
		for _, line := range strings.Split(normalized, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				currentGroup = strings.Trim(line, "[]")
				continue
			}
			for _, legacy := range legacyClassPatterns {
				if line == "ExceptionPattern="+legacy && strings.HasPrefix(currentGroup, "Windeco Exception") {
					if write("breezerc", currentGroup, "ExceptionPattern", exactSizeClassPattern) == nil {
						changed = true
						data, _ = os.ReadFile(filepath.Join(home, ".config", "breezerc"))
						normalized = strings.ReplaceAll(string(data), `\\`, `\`)
					}
				}
			}
		}
	}
	if !strings.Contains(normalized, exactSizeClassPattern) {
		nextIndex := 0
		for _, match := range regexp.MustCompile(`\[Windeco Exception (\d+)\]`).FindAllStringSubmatch(string(data), -1) {
			if index, err := strconv.Atoi(match[1]); err == nil && index >= nextIndex {
				nextIndex = index + 1
			}
		}
		group := fmt.Sprintf("Windeco Exception %d", nextIndex)
		settings := [][2]string{
			{"Enabled", "true"},
			{"ExceptionType", "0"}, // 0 = match window class name
			{"ExceptionPattern", exactSizeClassPattern},
			{"HideTitleBar", "true"},
			{"Mask", "0"},
		}
		for _, setting := range settings {
			if err := write("breezerc", group, setting[0], setting[1]); err != nil {
				return
			}
		}
		changed = true
	}

	if !changed {
		return
	}
	// Reload KWin so the first window already comes up without a title bar.
	if gdbus, err := exec.LookPath("gdbus"); err == nil {
		_ = exec.Command(gdbus, "call", "--session", "--dest", "org.kde.KWin", "--object-path", "/KWin", "--method", "org.kde.KWin.reconfigure").Run()
	} else if dbusSend, err := exec.LookPath("dbus-send"); err == nil {
		_ = exec.Command(dbusSend, "--session", "--type=method_call", "--dest=org.kde.KWin", "/KWin", "org.kde.KWin.reconfigure").Run()
	}
}

func showFatalDialog(message string) {
	if path, err := exec.LookPath("kdialog"); err == nil {
		_ = exec.Command(path, "--error", message, "--title", "ExactSize").Run()
		return
	}
	if path, err := exec.LookPath("zenity"); err == nil {
		_ = exec.Command(path, "--error", "--title=ExactSize", "--text="+message).Run()
	}
}

func showWarningDialog(message string) {
	if path, err := exec.LookPath("kdialog"); err == nil {
		_ = exec.Command(path, "--sorry", message, "--title", "ExactSize").Run()
		return
	}
	if path, err := exec.LookPath("zenity"); err == nil {
		_ = exec.Command(path, "--warning", "--title=ExactSize", "--text="+message).Run()
		return
	}
	if path, err := exec.LookPath("xmessage"); err == nil {
		_ = exec.Command(path, "-center", "-title", "ExactSize", message).Run()
	}
}
