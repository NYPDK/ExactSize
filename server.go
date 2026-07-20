package main

import (
	"crypto/subtle"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
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
	compare  *compareAssets
}

const comparePreviewPrefix = "exactsize-compare-"

type AppStatus struct {
	Version          string        `json:"version"`
	FFmpegVersion    string        `json:"ffmpegVersion"`
	Encoders         []EncoderInfo `json:"encoders"`
	AudioEncoders    []string      `json:"audioEncoders"`
	NativeDialog     bool          `json:"nativeDialog"`
	Frameless        bool          `json:"frameless"`
	DefaultOutputDir string        `json:"defaultOutputDir"`
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
// original is found again by name and size. The home directory goes last:
// its recursive walk is the noisiest, and it must not starve the
// removable-drive roots of the shared search budget.
func dropSearchDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var dirs []string
	seen := map[string]bool{}
	appendDir := func(dir string) {
		dir = filepath.Clean(dir)
		if dir != "." && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	for _, key := range []string{"VIDEOS", "DOWNLOAD", "DESKTOP", "DOCUMENTS", "PICTURES", "MUSIC"} {
		// xdg-user-dirs records a disabled folder as $HOME itself; the home
		// directory joins the list separately, at the end.
		if dir := xdgUserDir(key, home); filepath.Clean(dir) != home {
			appendDir(dir)
		}
	}
	// udisks mounts removable drives under /run/media/<user> (Debian-style
	// systems use /media/<user>); the user name matches the home directory.
	for _, parent := range []string{filepath.Join("/run/media", filepath.Base(home)), filepath.Join("/media", filepath.Base(home))} {
		for _, mount := range mountedMediaDirs(parent) {
			appendDir(mount)
		}
	}
	appendDir(home)
	return dirs
}

// mountedMediaDirs lists each mounted drive directly below base; the drives
// themselves are searched recursively later, like any other root.
func mountedMediaDirs(base string) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(base, entry.Name()))
		}
	}
	return dirs
}

// locateOriginalFile recovers the on-disk path of a dropped file. The
// freedesktop recent-documents list is consulted first because it records
// exact paths — resolving drops from anywhere, including places the
// directory search would never reach — and only then are the likely
// directories searched by name and size.
func locateOriginalFile(name string, size int64) string {
	if path := locateRecentFile(recentlyUsedPath(), name, size); path != "" {
		return path
	}
	return locateFileIn(dropSearchDirs(), name, size)
}

// recentlyUsedPath is the freedesktop shared recent-documents list. GTK
// applications have always written it, and KDE joined with Frameworks 5.93,
// so it covers both desktop families without extra dependencies.
func recentlyUsedPath() string {
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "recently-used.xbel")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "recently-used.xbel")
}

// locateRecentFile scans an XBEL recent-documents list for a bookmark whose
// base name and current on-disk size match the dropped file. Later entries
// are more recent, so the list is read backwards and the copy the user
// touched last wins.
func locateRecentFile(xbelPath, name string, size int64) string {
	base := sanitizeDropName(name)
	if base == "" || size <= 0 || xbelPath == "" {
		return ""
	}
	// A pathologically large history must not stall the drop; the directory
	// search still runs without this shortcut.
	if info, err := os.Stat(xbelPath); err != nil || info.Size() > 16<<20 {
		return ""
	}
	data, err := os.ReadFile(xbelPath)
	if err != nil {
		return ""
	}
	var document struct {
		Bookmarks []struct {
			Href string `xml:"href,attr"`
		} `xml:"bookmark"`
	}
	if err := xml.Unmarshal(data, &document); err != nil {
		return ""
	}
	for i := len(document.Bookmarks) - 1; i >= 0; i-- {
		parsed, err := url.Parse(document.Bookmarks[i].Href)
		if err != nil || parsed.Scheme != "file" || parsed.Path == "" {
			continue
		}
		candidate := filepath.FromSlash(parsed.Path)
		if filepath.Base(candidate) != base {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Size() == size {
			return candidate
		}
	}
	return ""
}

// The recursive pass of the drop search is bounded twice over: directories
// more than dropSearchMaxDepth levels below a root are pruned, and all roots
// share one wall-clock budget so /api/locate answers promptly even on
// enormous drives. Past either bound the file is reported as not found,
// exactly as before the recursive search existed.
const (
	dropSearchMaxDepth = 4
	dropSearchBudget   = time.Second
)

// locateFileIn finds a file with the dropped file's base name and exact byte
// size under dirs. The top level of every directory is checked first — one
// stat each, and the overwhelmingly common drop source — so a top-level
// match in a late root always beats a nested match in an early one; only
// then is each root walked recursively in priority order.
func locateFileIn(dirs []string, name string, size int64) string {
	base := sanitizeDropName(name)
	if base == "" || size <= 0 {
		return ""
	}
	for _, dir := range dirs {
		candidate := filepath.Join(dir, base)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Size() == size {
			return candidate
		}
	}
	// Each root gets an equal share of the remaining budget, and roots that
	// finish early donate their leftover time to the ones after them. A huge
	// early root can therefore never starve the later ones — notably the
	// removable drives, which sit behind the XDG folders.
	deadline := time.Now().Add(dropSearchBudget)
	for i, dir := range dirs {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if found := searchDirTree(dir, base, size, time.Now().Add(remaining/time.Duration(len(dirs)-i))); found != "" {
			return found
		}
	}
	return ""
}

// searchDirTree looks for base with the exact size below root, breadth
// first: users drop files that sit a level or two down, and a depth-first
// walk of a large drive would exhaust the budget inside whichever huge tree
// happens to sort first. Hidden directories and well-known noise trees are
// pruned, symlinked directories are not followed, and unreadable directories
// are skipped rather than aborting: a permission error somewhere must not
// hide a match elsewhere.
func searchDirTree(root, base string, size int64, deadline time.Time) string {
	type pending struct {
		path  string
		depth int
	}
	queue := []pending{{filepath.Clean(root), 0}}
	for len(queue) > 0 {
		if !time.Now().Before(deadline) {
			return ""
		}
		dir := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				if dir.depth < dropSearchMaxDepth && !skipDropSearchDir(name) {
					queue = append(queue, pending{filepath.Join(dir.path, name), dir.depth + 1})
				}
				continue
			}
			if name != base || !entry.Type().IsRegular() {
				continue
			}
			if info, err := entry.Info(); err == nil && info.Size() == size {
				return filepath.Join(dir.path, name)
			}
		}
	}
	return ""
}

// skipDropSearchDir prunes trees that cannot plausibly hold a user's video:
// hidden directories (which also covers .git and caches), dependency trees,
// and Linux and Windows filesystem plumbing (external drives are frequently
// NTFS-formatted).
func skipDropSearchDir(name string) bool {
	switch name {
	case "node_modules", "lost+found", "$RECYCLE.BIN", "System Volume Information":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// sanitizeDropName reduces a client-supplied file name to its base name;
// traversal segments must never influence where the search looks.
func sanitizeDropName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "." || base == "/" {
		return ""
	}
	return base
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
	cleanupStaleComparePreviews()
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
	mux.HandleFunc("POST /api/compare/open", a.auth(a.handleCompareOpen))
	mux.HandleFunc("GET /api/compare/media/{side}", a.authMedia(a.handleCompareMedia))
	mux.HandleFunc("GET /api/compare/storyboard", a.authMedia(a.handleCompareStoryboard))
	mux.HandleFunc("GET /api/compare/storyboard/manifest", a.auth(a.handleCompareStoryboardManifest))
	mux.HandleFunc("POST /api/compare/convert", a.auth(a.handleCompareConvertStart))
	mux.HandleFunc("GET /api/compare/convert/{side}", a.auth(a.handleCompareConvertStatus))
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self'; connect-src 'self'; font-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
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

// authMedia also accepts the session token as a query parameter: <video> and
// background-image requests cannot attach custom headers.
func (a *App) authMedia(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-ExactSize-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(a.token)) != 1 {
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

	a.teardownCompareAssets()

	a.mu.Lock()
	if a.job != nil && !a.job.isTerminal() {
		a.mu.Unlock()
		writeError(w, http.StatusConflict, "another encode is already running")
		return
	}
	job := newJob(request)
	a.job = job
	a.mu.Unlock()

	go func() {
		job.run(a.ffmpeg, a.ffprobe)
		a.prepareCompareAssets(job)
	}()
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

func (a *App) currentComparisonJob() (*Job, int, string) {
	a.mu.RLock()
	job := a.job
	a.mu.RUnlock()
	if job == nil {
		return nil, http.StatusNotFound, "no completed compression is available to compare"
	}
	snapshot := job.snapshot()
	if snapshot.State != "completed" || job.request.Remux || job.request.MuxAudio {
		return nil, http.StatusConflict, "comparison is available only after a successful compression"
	}
	return job, 0, ""
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
	a.mu.Lock()
	job := a.job
	uploads := append([]string(nil), a.uploads...)
	a.uploads = nil
	a.mu.Unlock()
	if job != nil && !job.isTerminal() {
		job.cancel()
	}
	for _, path := range uploads {
		_ = os.Remove(path)
	}
	a.teardownCompareAssets()
}

func cleanupStaleComparePreviews() {
	directories, _ := filepath.Glob(filepath.Join(os.TempDir(), comparePreviewPrefix+"*"))
	for _, dir := range directories {
		name := strings.TrimPrefix(filepath.Base(dir), comparePreviewPrefix)
		pidText, _, found := strings.Cut(name, "-")
		if pid, err := strconv.Atoi(pidText); found && err == nil {
			if processIsRunning(pid) {
				continue
			}
			_ = os.RemoveAll(dir)
			continue
		}
		if info, err := os.Stat(dir); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(dir)
		}
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

var resizeUpdate = fmt.Sprintf(
	`target.frameGeometry = { x: start.x, y: start.y, width: Math.max(%d, start.width + dx), height: Math.max(%d, start.height + dy) };`,
	minimumWindowWidth,
	minimumWindowHeight,
)

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
