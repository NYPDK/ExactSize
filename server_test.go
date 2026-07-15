package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, behavior := range []string{"function progressMetric(job)", `label: "Video"`, `details.push(formatBitrate(bitrate))`, "details.push(`${trimNumber(fps, 2)} fps`)"} {
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
