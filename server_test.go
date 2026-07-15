package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrameRateSliderUsesWholeNumberSteps(t *testing.T) {
	html, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="frameRate" type="range" min="5" max="60" step="1"`) {
		t.Fatal("frame-rate slider must advance in whole-number steps")
	}

	javascript, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(javascript)
	if !strings.Contains(script, "Math.ceil(sourceFPS)") || !strings.Contains(script, "return Math.round(selected)") {
		t.Fatal("fractional source rates must keep a source position while reduced rates stay whole numbers")
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
