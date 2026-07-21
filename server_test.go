package main

import (
	"net/url"
	"os"
	"path/filepath"
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

// Desktop file managers can commit recently-used.xbel just after dispatching
// the drop event. The locator must reread it once before falling back to a
// temporary upload, which would lose the source directory for output naming.
func TestLocateOriginalFileRetriesDelayedRecentDocument(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "outside-search-roots")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDir, "cold-drop.mp4")
	if err := os.WriteFile(source, make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	recentDir := t.TempDir()
	recentPath := filepath.Join(recentDir, "recently-used.xbel")
	href := (&url.URL{Scheme: "file", Path: source}).String()
	document := `<xbel version="1.0"><bookmark href="` + href + `"/></xbel>`
	written := make(chan error, 1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		written <- os.WriteFile(recentPath, []byte(document), 0o644)
	}()

	got := locateOriginalFileIn("cold-drop.mp4", 8192, recentPath, []string{t.TempDir()}, 30*time.Millisecond)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if got != source {
		t.Fatalf("locateOriginalFileIn = %q, want delayed recent path %q", got, source)
	}
}
