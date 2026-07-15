package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type App struct {
	ffmpeg  string
	ffprobe string
	token   string
	web     fs.FS

	statusOnce   sync.Once
	status       AppStatus
	statusErr    error
	vaapiDevices map[string]string

	mu       sync.RWMutex
	job      *Job
	shutdown func()
	uploads  []string
}

type AppStatus struct {
	Version       string        `json:"version"`
	FFmpegVersion string        `json:"ffmpegVersion"`
	Encoders      []EncoderInfo `json:"encoders"`
	AudioEncoders []string      `json:"audioEncoders"`
	NativeDialog  bool          `json:"nativeDialog"`
	Frameless     bool          `json:"frameless"`
	DefaultOutputDir string     `json:"defaultOutputDir"`
}

// xdgUserDir reads one entry from ~/.config/user-dirs.dirs (for example
// XDG_VIDEOS_DIR="$HOME/Videos") and expands $HOME.
func xdgUserDir(key, home string) string {
	data, err := os.ReadFile(filepath.Join(home, ".config", "user-dirs.dirs"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "XDG_"+key+"_DIR=") {
			continue
		}
		value := strings.Trim(strings.SplitN(line, "=", 2)[1], `"`)
		value = strings.ReplaceAll(value, "$HOME", home)
		if info, err := os.Stat(value); err == nil && info.IsDir() {
			return value
		}
	}
	return ""
}

// dropSearchDirs are the places a dragged-in video most plausibly came from,
// in priority order. Browsers hand us file content without a path, so the
// original is found again by name and size.
func dropSearchDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var dirs []string
	for _, key := range []string{"VIDEOS", "DOWNLOAD", "DESKTOP"} {
		if dir := xdgUserDir(key, home); dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return append(dirs, home)
}

func locateOriginalFile(name string, size int64) string {
	return locateFileIn(dropSearchDirs(), name, size)
}

func locateFileIn(dirs []string, name string, size int64) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == "/" || size <= 0 {
		return ""
	}
	for _, dir := range dirs {
		candidate := filepath.Join(dir, base)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Size() == size {
			return candidate
		}
	}
	return ""
}

func defaultOutputDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if videos := xdgUserDir("VIDEOS", home); videos != "" {
		return videos
	}
	return home
}

func (a *App) handleLocate(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": locateOriginalFile(request.Name, request.Size)})
}

type dialogResponse struct {
	Path     string `json:"path,omitempty"`
	Canceled bool   `json:"canceled,omitempty"`
	Fallback bool   `json:"fallback,omitempty"`
}

func newApp(ffmpeg, ffprobe, token string, web fs.FS) *App {
	return &App{ffmpeg: ffmpeg, ffprobe: ffprobe, token: token, web: web}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", a.staticHandler())
	mux.Handle("GET /styles.css", a.staticHandler())
	mux.Handle("GET /app.js", a.staticHandler())
	mux.Handle("GET /icon.svg", a.staticHandler())
	mux.HandleFunc("GET /api/status", a.auth(a.handleStatus))
	mux.HandleFunc("POST /api/dialog/open", a.auth(a.handleOpenDialog))
	mux.HandleFunc("POST /api/dialog/save", a.auth(a.handleSaveDialog))
	mux.HandleFunc("POST /api/upload", a.auth(a.handleUpload))
	mux.HandleFunc("POST /api/locate", a.auth(a.handleLocate))
	mux.HandleFunc("POST /api/probe", a.auth(a.handleProbe))
	mux.HandleFunc("POST /api/jobs", a.auth(a.handleStartJob))
	mux.HandleFunc("GET /api/jobs/current", a.auth(a.handleCurrentJob))
	mux.HandleFunc("DELETE /api/jobs/current", a.auth(a.handleCancelJob))
	mux.HandleFunc("POST /api/reveal", a.auth(a.handleReveal))
	mux.HandleFunc("POST /api/window/{action}", a.auth(a.handleWindowAction))
	mux.HandleFunc("POST /api/quit", a.auth(a.handleQuit))
	return securityHeaders(mux)
}

func (a *App) staticHandler() http.Handler {
	return http.FileServer(http.FS(a.web))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-ExactSize-Token") != a.token {
			writeError(w, http.StatusForbidden, "invalid application session")
			return
		}
		next(w, r)
	}
}

// inspectRuntime probes FFmpeg and the local GPU once per launch; hardware
// probes cost a few hundred milliseconds each, so the result is cached.
func (a *App) inspectRuntime() (AppStatus, error) {
	a.statusOnce.Do(func() {
		a.status, a.vaapiDevices, a.statusErr = inspectFFmpeg(a.ffmpeg)
	})
	return a.status, a.statusErr
}

func (a *App) handleStatus(w http.ResponseWriter, _ *http.Request) {
	status, err := a.inspectRuntime()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status.Version = version
	status.NativeDialog = hasNativeDialog()
	status.Frameless = hasKWinScripting()
	status.DefaultOutputDir = defaultOutputDir()
	writeJSON(w, http.StatusOK, status)
}

func (a *App) handleOpenDialog(w http.ResponseWriter, r *http.Request) {
	var request struct {
		StartDir string `json:"startDir"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&request)
	path, canceled, err := openVideoDialog(request.StartDir)
	if errors.Is(err, errNoDialog) {
		writeJSON(w, http.StatusOK, dialogResponse{Fallback: true})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dialogResponse{Path: path, Canceled: canceled})
}

func (a *App) handleSaveDialog(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Suggested string `json:"suggested"`
		Container string `json:"container"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	path, canceled, err := saveVideoDialog(request.Suggested, request.Container)
	if errors.Is(err, errNoDialog) {
		writeJSON(w, http.StatusOK, dialogResponse{Fallback: true, Path: request.Suggested})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dialogResponse{Path: path, Canceled: canceled})
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected a multipart video upload")
		return
	}
	part, err := nextFilePart(reader)
	if err != nil {
		writeError(w, http.StatusBadRequest, "no video file was provided")
		return
	}
	defer part.Close()

	ext := strings.ToLower(filepath.Ext(filepath.Base(part.FileName())))
	file, err := os.CreateTemp("", "exactsize-input-*"+ext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a temporary input file")
		return
	}
	path := file.Name()
	if _, err := io.Copy(file, part); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		writeError(w, http.StatusInternalServerError, "could not copy the selected video")
		return
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		writeError(w, http.StatusInternalServerError, "could not finish the temporary input file")
		return
	}

	a.mu.Lock()
	a.uploads = append(a.uploads, path)
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"path": path, "name": filepath.Base(part.FileName())})
}

func nextFilePart(reader *multipart.Reader) (*multipart.Part, error) {
	for {
		part, err := reader.NextPart()
		if err != nil {
			return nil, err
		}
		if part.FileName() != "" {
			return part, nil
		}
		_ = part.Close()
	}
}

func (a *App) handleProbe(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := probeVideo(r.Context(), a.ffprobe, request.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (a *App) handleStartJob(w http.ResponseWriter, r *http.Request) {
	var request EncodeRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateEncodeRequest(request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !request.Remux {
		status, err := a.inspectRuntime()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		encoderAvailable := false
		for _, encoder := range status.Encoders {
			if encoder.ID == request.Encoder {
				encoderAvailable = true
				break
			}
		}
		if !encoderAvailable {
			writeError(w, http.StatusBadRequest, "the selected encoder is not available on this system")
			return
		}
		request.VAAPIDevice = a.vaapiDevices[request.Encoder]
	}

	a.mu.Lock()
	if a.job != nil && !a.job.isTerminal() {
		a.mu.Unlock()
		writeError(w, http.StatusConflict, "another encode is already running")
		return
	}
	job := newJob(request)
	a.job = job
	a.mu.Unlock()

	go job.run(a.ffmpeg, a.ffprobe)
	writeJSON(w, http.StatusAccepted, job.snapshot())
}

func (a *App) handleCurrentJob(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	job := a.job
	a.mu.RUnlock()
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]any{"state": "idle"})
		return
	}
	writeJSON(w, http.StatusOK, job.snapshot())
}

func (a *App) handleCancelJob(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	job := a.job
	a.mu.RUnlock()
	if job == nil || job.isTerminal() {
		writeJSON(w, http.StatusOK, map[string]string{"state": "idle"})
		return
	}
	job.cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{"state": "canceling"})
}

func (a *App) cancelCurrentJob() {
	a.mu.RLock()
	job := a.job
	a.mu.RUnlock()
	if job != nil && !job.isTerminal() {
		job.cancel()
	}
	for _, path := range a.uploads {
		_ = os.Remove(path)
	}
}

func (a *App) handleReveal(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := revealFile(request.Path); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Window controls: without a native title bar (KDE hides it via a window
// rule), the page header drives compositor-side interactive move and resize
// through KWin's scripting interface. Each action runs a one-shot script
// guarded by the window title, so it never touches another application.
func hasKWinScripting() bool {
	if !strings.Contains(os.Getenv("XDG_CURRENT_DESKTOP"), "KDE") {
		return false
	}
	_, err := exec.LookPath("gdbus")
	return err == nil
}

// followPlugin is the plugin name of the drag/resize follower script; only
// one can be active at a time.
const followPlugin = "exactsize-follow"

// followScript makes the app window track the cursor from its grab offset,
// like a title-bar drag. KWin's real interactive move is not reachable from
// scripts (performMouseCommand is not exposed, and slotWindowMove warps the
// pointer to the window center), so the page reports press and release and
// this script follows the cursorPosChanged signal in between: one update per
// input event, the same cadence as a native move. A timer tears the follower
// down after 60 seconds in case the release notification never arrives.
func followScript(update string) string {
	return `var target = workspace.activeWindow || workspace.activeClient;
if (target && target.caption === "ExactSize") {
  var startCursor = { x: workspace.cursorPos.x, y: workspace.cursorPos.y };
  var g = target.frameGeometry;
  var start = { x: g.x, y: g.y, width: g.width, height: g.height };
  var handler = function() {
    var dx = workspace.cursorPos.x - startCursor.x;
    var dy = workspace.cursorPos.y - startCursor.y;
    ` + update + `
  };
  workspace.cursorPosChanged.connect(handler);
  var failsafe = new QTimer();
  failsafe.interval = 60000;
  failsafe.singleShot = true;
  failsafe.timeout.connect(function() { workspace.cursorPosChanged.disconnect(handler); });
  failsafe.start();
}`
}

// KWin's plain-JS environment has no Qt.rect; assigning an object literal to
// frameGeometry works.
const moveUpdate = `target.frameGeometry = { x: start.x + dx, y: start.y + dy, width: start.width, height: start.height };`
const resizeUpdate = `target.frameGeometry = { x: start.x, y: start.y, width: Math.max(1040, start.width + dx), height: Math.max(620, start.height + dy) };`

func (a *App) handleWindowAction(w http.ResponseWriter, r *http.Request) {
	if !hasKWinScripting() {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": false})
		return
	}
	var err error
	switch r.PathValue("action") {
	case "move-start":
		// A real compositor move via X11; the cursor-following KWin script
		// remains as the fallback for non-X11 windows.
		if err = startX11MoveResize(netWMMoveResizeMove); err != nil {
			err = startKWinFollowScript(followScript(moveUpdate))
		}
	case "resize-start":
		if err = startX11MoveResize(netWMMoveResizeSizeBottomRight); err != nil {
			err = startKWinFollowScript(followScript(resizeUpdate))
		}
	case "move-end", "resize-end":
		// The compositor ends an X11 interactive move itself; this only tears
		// down a fallback follower if one is active.
		err = unloadKWinScript(followPlugin)
	case "minimize":
		err = runKWinScript(`var target = workspace.activeWindow || workspace.activeClient;
if (target && target.caption === "ExactSize") { target.minimized = true; }`)
	default:
		writeError(w, http.StatusBadRequest, "unknown window action")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "window control is unavailable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// loadAndRunKWinScript loads the script body under pluginName and runs it,
// returning without unloading; callers decide the script's lifetime.
func loadAndRunKWinScript(body, pluginName string) error {
	gdbus, err := exec.LookPath("gdbus")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "exactsize-kwin-*.js")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.WriteString(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	output, err := exec.Command(gdbus, "call", "--session", "--dest", "org.kde.KWin",
		"--object-path", "/Scripting", "--method", "org.kde.kwin.Scripting.loadScript", path, pluginName).Output()
	if err != nil {
		return fmt.Errorf("load KWin script: %w", err)
	}
	match := regexp.MustCompile(`-?\d+`).FindString(string(output))
	if match == "" || strings.HasPrefix(match, "-") {
		return errors.New("KWin rejected the script")
	}
	scriptPath := "/Scripting/Script" + match
	if err := exec.Command(gdbus, "call", "--session", "--dest", "org.kde.KWin",
		"--object-path", scriptPath, "--method", "org.kde.kwin.Script.run").Run(); err != nil {
		_ = unloadKWinScript(pluginName)
		return fmt.Errorf("run KWin script: %w", err)
	}
	return nil
}

func unloadKWinScript(pluginName string) error {
	gdbus, err := exec.LookPath("gdbus")
	if err != nil {
		return err
	}
	return exec.Command(gdbus, "call", "--session", "--dest", "org.kde.KWin",
		"--object-path", "/Scripting", "--method", "org.kde.kwin.Scripting.unloadScript", pluginName).Run()
}

// startKWinFollowScript replaces any active follower with a fresh one.
func startKWinFollowScript(body string) error {
	_ = unloadKWinScript(followPlugin)
	return loadAndRunKWinScript(body, followPlugin)
}

// runKWinScript executes a one-shot script and unloads it immediately.
func runKWinScript(body string) error {
	pluginName := fmt.Sprintf("exactsize-window-%d", time.Now().UnixNano())
	if err := loadAndRunKWinScript(body, pluginName); err != nil {
		return err
	}
	return unloadKWinScript(pluginName)
}

func (a *App) handleQuit(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if a.shutdown != nil {
		go func() {
			time.Sleep(100 * time.Millisecond)
			a.shutdown()
		}()
	}
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

var errNoDialog = errors.New("no supported native file dialog")

func hasNativeDialog() bool {
	for _, name := range []string{"kdialog", "zenity", "yad"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}

func openVideoDialog(startDir string) (string, bool, error) {
	if startDir == "" {
		startDir, _ = os.UserHomeDir()
	}
	filter := "Video files (*.mp4 *.mkv *.webm *.mov *.avi *.m4v *.mts *.m2ts *.ts *.wmv *.flv);;All files (*)"
	if path, err := exec.LookPath("kdialog"); err == nil {
		return runDialog(path, "--getopenfilename", startDir, filter, "--title", "Select input video")
	}
	if path, err := exec.LookPath("zenity"); err == nil {
		return runDialog(path, "--file-selection", "--title=Select input video", "--file-filter=Video files | *.mp4 *.mkv *.webm *.mov *.avi *.m4v *.mts *.m2ts *.ts *.wmv *.flv", "--file-filter=All files | *")
	}
	if path, err := exec.LookPath("yad"); err == nil {
		return runDialog(path, "--file-selection", "--title=Select input video")
	}
	return "", false, errNoDialog
}

func saveVideoDialog(suggested, container string) (string, bool, error) {
	if suggested == "" {
		home, _ := os.UserHomeDir()
		suggested = filepath.Join(home, "Videos", "compressed."+containerExtension(container))
	}
	ext := containerExtension(container)
	if path, err := exec.LookPath("kdialog"); err == nil {
		// kdialog has no --confirm-overwrite option (that is zenity's); an
		// unknown option makes it exit non-zero, which reads as "canceled".
		filter := strings.ToUpper(ext) + " video (*." + ext + ");;All files (*)"
		return runDialog(path, "--getsavefilename", suggested, filter, "--title", "Choose output file")
	}
	if path, err := exec.LookPath("zenity"); err == nil {
		return runDialog(path, "--file-selection", "--save", "--confirm-overwrite", "--title=Choose output file", "--filename="+suggested)
	}
	if path, err := exec.LookPath("yad"); err == nil {
		return runDialog(path, "--file-selection", "--save", "--confirm-overwrite", "--title=Choose output file", "--filename="+suggested)
	}
	return "", false, errNoDialog
}

func runDialog(command string, args ...string) (string, bool, error) {
	output, err := exec.Command(command, args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() != 0 {
			return "", true, nil
		}
		return "", false, err
	}
	path := strings.TrimSpace(string(output))
	return path, path == "", nil
}

func revealFile(path string) error {
	if path == "" {
		return errors.New("no output file is available")
	}
	if _, err := os.Stat(path); err != nil {
		return errors.New("the output file no longer exists")
	}
	if runtime.GOOS != "linux" {
		return errors.New("revealing files is currently supported on Linux only")
	}
	if command, err := exec.LookPath("dbus-send"); err == nil {
		uri := "file://" + filepath.ToSlash(path)
		cmd := exec.Command(command, "--session", "--dest=org.freedesktop.FileManager1", "--type=method_call", "/org/freedesktop/FileManager1", "org.freedesktop.FileManager1.ShowItems", "array:string:"+uri, "string:")
		if err := cmd.Start(); err == nil {
			return nil
		}
	}
	command, err := exec.LookPath("xdg-open")
	if err != nil {
		return errors.New("xdg-open is not available")
	}
	return exec.Command(command, filepath.Dir(path)).Start()
}

func parseInt64(value string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return n
}
