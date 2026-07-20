package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFrameRateRangeUsesTwoWholeNumberHandles(t *testing.T) {
	html, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	markup := string(html)
	for _, handle := range []string{"frameRateMinimum", "frameRateMaximum"} {
		if !strings.Contains(markup, `id="`+handle+`" type="range" min="5" max="60" step="1"`) {
			t.Fatalf("%s must be a whole-number range handle", handle)
		}
	}

	javascript, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(javascript)
	for _, behavior := range []string{"Math.ceil(sourceFPS)", "requestedMinimumOutputFPS", "selectedMinimum <= absoluteMinimum", "const startProgress = strict ? 0", "minimumOutputFps: requestedMinimumOutputFPS()"} {
		if !strings.Contains(script, behavior) {
			t.Fatalf("frame-rate range is missing %q behavior", behavior)
		}
	}
}

func TestDisabledFrameRateHandlesStayOpaque(t *testing.T) {
	css, err := webAssets.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	styles := string(css)
	if !strings.Contains(styles, ".frame-rate-range:disabled {\n  opacity: 1;\n}") {
		t.Fatal("disabled FPS handles must remain opaque while encoding locks the sliders")
	}
}

func TestStartingResolutionAndAutomaticFallbackAreSeparateControls(t *testing.T) {
	html, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	markup := string(html)
	for _, control := range []string{`id="resolution"`, `id="autoResolution" type="checkbox" checked`, `Automatic resolution`} {
		if !strings.Contains(markup, control) {
			t.Fatalf("resolution settings are missing %q", control)
		}
	}
	javascript, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(javascript)
	for _, behavior := range []string{"Source (${state.input.width} × ${sourceHeight})", "autoResolution: elements.autoResolution.checked"} {
		if !strings.Contains(script, behavior) {
			t.Fatalf("resolution controls are missing %q behavior", behavior)
		}
	}
}

func TestProgressPanelDoesNotRepeatEncoding(t *testing.T) {
	html, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="progressPassLabel"`) {
		t.Fatal("the progress metric needs a dynamic label")
	}
	javascript, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(javascript)
	for _, behavior := range []string{
		"function progressMetric(job)",
		`label: "Video"`,
		`details.push(formatBitrate(bitrate))`,
		"details.push(`${trimNumber(fps, 2)} fps`)",
		"function notifyCorrection(job)",
		"attempt <= 1",
		"showToast(message)",
		`elements.progressMessage.hidden = active`,
	} {
		if !strings.Contains(script, behavior) {
			t.Fatalf("progress panel is missing %q", behavior)
		}
	}
}

func TestCompletedCompressionOffersLargeSynchronizedComparison(t *testing.T) {
	html, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	markup := string(html)
	for _, control := range []string{
		`id="compareButton"`,
		`class="compare-overlay"`,
		`aria-modal="true"`,
		`<video class="compare-frame compare-original" id="compareOriginalVideo" muted playsinline preload="auto">`,
		`<video class="compare-frame compare-compressed" id="compareCompressedVideo" playsinline preload="auto">`,
		`id="comparePlay"`,
		`id="compareMute"`,
		`id="compareSlider" type="range"`,
		`id="compareHoverPreview"`,
		`id="compareHoverThumb"`,
		`id="compareHoverTime"`,
		`id="compareTimeline" type="range"`,
	} {
		if !strings.Contains(markup, control) {
			t.Fatalf("comparison UI is missing %q", control)
		}
	}

	javascript, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(javascript)
	for _, behavior := range []string{
		"async function openCompare()",
		"function closeCompare()",
		"function toggleComparePlayback()",
		"function toggleCompareMute()",
		"function finishCompareOpen()",
		"function seekCompare(seconds)",
		"function startCompareSync()",
		"function updateCompareClock()",
		"function chooseCompareSource(side)",
		"function attachCompareSource(side, variant)",
		"async function startCompareConvert(side)",
		"async function pollCompareConvert(side)",
		"function initCompareStoryboard(manifest)",
		"function loadCompareStoryboardImage()",
		"function previewCompareTimeline(event)",
		"/api/compare/open",
		"/api/compare/media/",
		"/api/compare/convert",
		"/api/compare/storyboard",
		"canPlayType",
		"requestAnimationFrame",
		".currentTime = master.currentTime",
		"backgroundPosition",
		`addEventListener("ended"`,
		`addEventListener("pointermove", previewCompareTimeline)`,
		`addEventListener("pointerleave", hideCompareHoverPreview)`,
		"The compressed output has no audio track",
		`document.body.classList.add("compare-open")`,
		`document.body.classList.remove("compare-open")`,
		`startsWith("Video compressed successfully")`,
	} {
		if !strings.Contains(script, behavior) {
			t.Fatalf("comparison behavior is missing %q", behavior)
		}
	}

	// The stills-era machinery must be gone: no per-position fetches, no
	// object URLs, no frame endpoints.
	for _, removed := range []string{
		"/api/compare/frame",
		"fetchCompareFrame",
		"createDecodedCompareFrameURLs",
		"createDecodedCompareHoverURL",
		"loadCompareFrames",
		"loadCompareHoverFrames",
		"scheduleCompareFrame",
		"scheduleCompareHoverFrame",
		"compareHoverObjectURLs",
		"compareFrameObjectURLs",
	} {
		if strings.Contains(script, removed) {
			t.Fatalf("stills-era comparison machinery %q must be removed", removed)
		}
	}
	if strings.Contains(markup, `id="compareHoverFrame"`) || strings.Contains(markup, `id="compareOriginalFrame"`) {
		t.Fatal("the stills-era img elements must be replaced by video elements")
	}

	// Hover previews are storyboard lookups: the pointermove handler may not
	// perform network requests.
	hoverStart := strings.Index(script, "function previewCompareTimeline(event)")
	if hoverStart < 0 {
		t.Fatal("could not find the timeline hover handler")
	}
	hoverEnd := strings.Index(script[hoverStart:], "\n}\n")
	if hoverEnd < 0 {
		t.Fatal("could not inspect the timeline hover handler")
	}
	hoverHandler := script[hoverStart : hoverStart+hoverEnd]
	for _, network := range []string{"fetch(", "api(", "await"} {
		if strings.Contains(hoverHandler, network) {
			t.Fatalf("timeline hover must stay a local storyboard lookup, found %q", network)
		}
	}

	css, err := webAssets.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	styles := string(css)
	if strings.Contains(markup, "compare-handle") || strings.Contains(styles, ".compare-handle") {
		t.Fatal("the comparison divider should remain a plain line without a center handle")
	}
	if strings.Contains(styles, "backdrop-filter:") {
		t.Fatal("fullscreen backdrop-filter can blank the app compositor")
	}
	for _, layout := range []string{
		".compare-overlay {",
		"padding: 24px;",
		"body.compare-open .app-shell {",
		"filter: blur(3px);",
		".compare-dialog {",
		"grid-template-rows: auto minmax(0, 1fr) auto;",
		"width: 100%;",
		"height: 100%;",
		"grid-template-columns: auto auto minmax(120px, 1fr) auto auto;",
		".compare-hover-preview {",
		"width: 192px;",
		".compare-hover-thumb {",
		".compare-hover-time {",
		".compare-play.playing .play-icon",
		".compare-mute.muted .sound-icon",
	} {
		if !strings.Contains(styles, layout) {
			t.Fatalf("playback comparison modal is missing %q", layout)
		}
	}
}

func TestStaleComparePreviewCleanupPreservesActiveProcess(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	active := filepath.Join(tempDir, comparePreviewPrefix+strconv.Itoa(os.Getpid())+"-active")
	stale := filepath.Join(tempDir, comparePreviewPrefix+strconv.Itoa(1<<30)+"-stale")
	legacy := filepath.Join(tempDir, comparePreviewPrefix+"legacy")
	for _, path := range []string{active, stale, legacy} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(legacy, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	cleanupStaleComparePreviews()
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active comparison previews should be preserved: %v", err)
	}
	for _, path := range []string{stale, legacy} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale comparison previews should be removed: %s (%v)", path, err)
		}
	}
}

func TestLocateFileIn(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeSized := func(dir, name string, size int) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	inSecond := writeSized(second, "clip.mp4", 4096)
	writeSized(second, "other.mp4", 100)

	dirs := []string{first, second}
	if got := locateFileIn(dirs, "clip.mp4", 4096); got != inSecond {
		t.Errorf("locateFileIn = %q, want %q", got, inSecond)
	}
	if got := locateFileIn(dirs, "clip.mp4", 5); got != "" {
		t.Errorf("size mismatch must not match, got %q", got)
	}
	if got := locateFileIn(dirs, "missing.mp4", 4096); got != "" {
		t.Errorf("missing file must not match, got %q", got)
	}
	// Priority: the first directory wins when both contain a match.
	inFirst := writeSized(first, "clip.mp4", 4096)
	if got := locateFileIn(dirs, "clip.mp4", 4096); got != inFirst {
		t.Errorf("priority order broken: got %q, want %q", got, inFirst)
	}
	// Path traversal in the client-supplied name must be neutralized.
	if got := locateFileIn(dirs, "../"+filepath.Base(first)+"/clip.mp4", 4096); got != inFirst {
		t.Errorf("traversal should reduce to the base name, got %q", got)
	}
}

func TestLocateFileInSearchesNestedDirectories(t *testing.T) {
	cases := []struct {
		name   string
		relDir string
		found  bool
	}{
		{"one level below the root", "footage", true},
		{"four levels below the root", "a/b/c/d", true},
		{"past the depth cap", "a/b/c/d/e", false},
		{"hidden directory", ".cache", false},
		{"dependency tree", "node_modules", false},
		{"linux filesystem plumbing", "lost+found", false},
		{"windows recycle bin", "$RECYCLE.BIN", false},
		{"windows volume metadata", "System Volume Information", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, filepath.FromSlash(test.relDir))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "clip.mp4")
			if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
			want := ""
			if test.found {
				want = path
			}
			if got := locateFileIn([]string{root}, "clip.mp4", 4096); got != want {
				t.Errorf("locateFileIn = %q, want %q", got, want)
			}
		})
	}
}

// A top-level match in any root beats a nested match everywhere: the top
// levels are the overwhelmingly common drop source and cost one stat each.
func TestLocateFileInPrefersTopLevelOverNested(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	write := func(dir string) string {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "clip.mp4")
		if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	nestedFirst := write(filepath.Join(first, "sub"))
	topSecond := write(second)
	dirs := []string{first, second}
	if got := locateFileIn(dirs, "clip.mp4", 4096); got != topSecond {
		t.Errorf("top-level match should win: got %q, want %q", got, topSecond)
	}
	// Among nested matches the earlier root keeps its priority.
	if err := os.Remove(topSecond); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(second, "sub"))
	if got := locateFileIn(dirs, "clip.mp4", 4096); got != nestedFirst {
		t.Errorf("nested priority order broken: got %q, want %q", got, nestedFirst)
	}
}

func TestSearchDirTreeStopsAtTheDeadline(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "clip.mp4")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := searchDirTree(root, "clip.mp4", 4096, time.Now().Add(time.Minute)); got != path {
		t.Errorf("searchDirTree = %q, want %q", got, path)
	}
	if got := searchDirTree(root, "clip.mp4", 4096, time.Now().Add(-time.Second)); got != "" {
		t.Errorf("an expired budget must end the search, got %q", got)
	}
}

func TestLocateRecentFile(t *testing.T) {
	dataDir := t.TempDir()
	fileDir := filepath.Join(t.TempDir(), "My Videos")
	if err := os.MkdirAll(fileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(fileDir, "clip.mp4")
	if err := os.WriteFile(clip, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	// The href is percent-encoded exactly the way GTK writes it, so the
	// space in the directory name exercises the decoding.
	href := (&url.URL{Scheme: "file", Path: clip}).String()
	xbel := filepath.Join(dataDir, "recently-used.xbel")
	document := `<?xml version="1.0" encoding="UTF-8"?>
<xbel version="1.0">
  <bookmark href="https://example.com/clip.mp4"/>
  <bookmark href="file:///nowhere/clip.mp4"/>
  <bookmark href="` + href + `"/>
</xbel>`
	if err := os.WriteFile(xbel, []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := locateRecentFile(xbel, "clip.mp4", 4096); got != clip {
		t.Errorf("locateRecentFile = %q, want %q", got, clip)
	}
	if got := locateRecentFile(xbel, "clip.mp4", 5); got != "" {
		t.Errorf("size mismatch must not match, got %q", got)
	}
	if got := locateRecentFile(xbel, "other.mp4", 4096); got != "" {
		t.Errorf("name mismatch must not match, got %q", got)
	}
	if got := locateRecentFile(filepath.Join(dataDir, "missing.xbel"), "clip.mp4", 4096); got != "" {
		t.Errorf("a missing history must not match, got %q", got)
	}

	// The newest matching entry (last in the document) wins over older ones.
	newer := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(newer, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	newerHref := (&url.URL{Scheme: "file", Path: newer}).String()
	document = `<xbel version="1.0"><bookmark href="` + href + `"/><bookmark href="` + newerHref + `"/></xbel>`
	if err := os.WriteFile(xbel, []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := locateRecentFile(xbel, "clip.mp4", 4096); got != newer {
		t.Errorf("the most recent entry should win: got %q, want %q", got, newer)
	}

	if err := os.WriteFile(xbel, []byte("<xbel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := locateRecentFile(xbel, "clip.mp4", 4096); got != "" {
		t.Errorf("a malformed history must not match, got %q", got)
	}
}

func TestMountedMediaDirs(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"BackupHDD", "USB"} {
		if err := os.Mkdir(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := mountedMediaDirs(base)
	want := []string{filepath.Join(base, "BackupHDD"), filepath.Join(base, "USB")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("mountedMediaDirs = %v, want %v", got, want)
	}
	if got := mountedMediaDirs(filepath.Join(base, "missing")); got != nil {
		t.Errorf("a missing mount parent must yield nothing, got %v", got)
	}
}

// dropSearchDirs must honor the configured XDG folders, normalize and list
// each root only once, and treat an XDG entry pointing at $HOME as disabled
// (xdg-user-dirs' sentinel) so the noisy home walk always stays last.
func TestDropSearchDirsDeduplicatesRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "Videos"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "XDG_VIDEOS_DIR=\"$HOME/Videos\"\nXDG_DOWNLOAD_DIR=\"$HOME/Videos/\"\nXDG_DESKTOP_DIR=\"$HOME/\"\n"
	if err := os.WriteFile(filepath.Join(home, ".config", "user-dirs.dirs"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	got := dropSearchDirs()
	want := []string{filepath.Join(home, "Videos"), home}
	if len(got) != len(want) {
		t.Fatalf("dropSearchDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dropSearchDirs = %v, want %v", got, want)
		}
	}
}

// A drop resolved through the recent-documents list works even when the file
// lives somewhere the directory search would never look.
func TestLocateOriginalFileUsesRecentDocuments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	elsewhere := filepath.Join(t.TempDir(), "archive")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(elsewhere, "clip.mp4")
	if err := os.WriteFile(clip, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	href := (&url.URL{Scheme: "file", Path: clip}).String()
	document := `<xbel version="1.0"><bookmark href="` + href + `"/></xbel>`
	if err := os.WriteFile(filepath.Join(dataDir, "recently-used.xbel"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := locateOriginalFile("clip.mp4", 4096); got != clip {
		t.Errorf("locateOriginalFile = %q, want %q", got, clip)
	}
}
