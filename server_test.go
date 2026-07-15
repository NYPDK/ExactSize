package main

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
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
		`id="compareOriginalFrame"`,
		`id="compareCompressedFrame"`,
		`id="compareSlider" type="range"`,
		`id="compareHoverPreview"`,
		`id="compareHoverFrame"`,
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
		"function openCompare()",
		"function closeCompare()",
		"function previewCompareTimeline(event)",
		"elements.compareTimeline.getBoundingClientRect()",
		"function scheduleCompareHoverFrame(seconds, delay = 70)",
		"function loadCompareHoverFrames(seconds)",
		"function createDecodedCompareHoverURL(seconds, signal)",
		`fetchCompareFrame("output", seconds, signal)`,
		"scheduleCompareHoverFrame(seconds, 70)",
		"elements.compareHoverPreview.hidden = false",
		"elements.compareHoverPreview.hidden = true",
		`addEventListener("pointermove", previewCompareTimeline)`,
		`addEventListener("pointerleave", hideCompareHoverPreview)`,
		"/api/compare/frame/${side}?time=${seconds.toFixed(3)}",
		"Loading matched frame at ${formatDuration(seconds)}…",
		"state.compareFrameAbortController?.abort()",
		"state.compareHoverAbortController?.abort()",
		"releaseCompareHoverURLs()",
		`document.body.classList.add("compare-open")`,
		`document.body.classList.remove("compare-open")`,
		`startsWith("Video compressed successfully")`,
	} {
		if !strings.Contains(script, behavior) {
			t.Fatalf("comparison behavior is missing %q", behavior)
		}
	}
	hoverStart := strings.Index(script, "function previewCompareTimeline(event)")
	if hoverStart < 0 {
		t.Fatal("could not find the timeline hover handler")
	}
	hoverEnd := strings.Index(script[hoverStart:], "\n}\n\nasync function fetchCompareFrame")
	if hoverEnd < 0 {
		t.Fatal("could not inspect the timeline hover handler")
	}
	hoverHandler := script[hoverStart : hoverStart+hoverEnd]
	for _, mainViewMutation := range []string{"scheduleCompareFrame(", "updateCompareTimelinePosition(", "elements.compareTimeline.value ="} {
		if strings.Contains(hoverHandler, mainViewMutation) {
			t.Fatalf("timeline hover must not mutate the main comparison through %q", mainViewMutation)
		}
	}
	hoverLoaderStart := strings.Index(script, "async function loadCompareHoverFrames(seconds)")
	if hoverLoaderStart < 0 {
		t.Fatal("could not find the hover thumbnail loader")
	}
	hoverLoaderEnd := strings.Index(script[hoverLoaderStart:], "\n}\n\nasync function loadCompareFrames")
	if hoverLoaderEnd < 0 {
		t.Fatal("could not inspect the hover thumbnail loader")
	}
	hoverLoader := script[hoverLoaderStart : hoverLoaderStart+hoverLoaderEnd]
	if strings.Contains(hoverLoader, "createDecodedCompareFrameURLs") || !strings.Contains(hoverLoader, "createDecodedCompareHoverURL") {
		t.Fatal("hover thumbnails must use the single compressed-frame decoder")
	}
	hoverDecoderStart := strings.Index(script, "async function createDecodedCompareHoverURL(seconds, signal)")
	if hoverDecoderStart < 0 {
		t.Fatal("could not find the hover thumbnail decoder")
	}
	hoverDecoderEnd := strings.Index(script[hoverDecoderStart:], "\n}\n\nfunction scheduleCompareHoverFrame")
	if hoverDecoderEnd < 0 {
		t.Fatal("could not inspect the hover thumbnail decoder")
	}
	hoverDecoder := script[hoverDecoderStart : hoverDecoderStart+hoverDecoderEnd]
	if strings.Contains(hoverDecoder, `fetchCompareFrame("input"`) || !strings.Contains(hoverDecoder, `fetchCompareFrame("output"`) {
		t.Fatal("hover thumbnails must request only the compressed output")
	}

	css, err := webAssets.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	styles := string(css)
	for _, playback := range []string{"<video", `id="comparePlay"`, "/api/compare/stream", ".play()"} {
		if strings.Contains(markup, playback) || strings.Contains(script, playback) {
			t.Fatalf("still-frame comparison retains playback behavior %q", playback)
		}
	}
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
		"grid-template-columns: minmax(120px, 1fr) auto auto;",
		".compare-hover-preview {",
		"width: 192px;",
		".compare-hover-stage {",
		".compare-hover-time {",
	} {
		if !strings.Contains(styles, layout) {
			t.Fatalf("large comparison modal is missing %q", layout)
		}
	}
	for _, removedSplitPreview := range []string{`id="compareHoverOriginal"`, `id="compareHoverCompressed"`, "compare-hover-divider"} {
		if strings.Contains(markup, removedSplitPreview) || strings.Contains(styles, removedSplitPreview) {
			t.Fatalf("hover preview must contain only the compressed frame, found %q", removedSplitPreview)
		}
	}
}

func TestCompareFramesAreScopedAuthenticatedAndGenerated(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "original.mp4")
	output := filepath.Join(dir, "compressed.mp4")
	if err := os.WriteFile(input, []byte("original-video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("compressed-video"), 0o600); err != nil {
		t.Fatal(err)
	}

	job := newJob(EncodeRequest{Input: input, Output: output})
	job.set(func(status *JobSnapshot) {
		status.State = "completed"
		status.Message = "Video compressed successfully"
	})
	callLog := filepath.Join(dir, "ffmpeg-args")
	t.Setenv("EXACTSIZE_COMPARE_TEST_LOG", callLog)
	fakeFFmpeg := filepath.Join(dir, "ffmpeg")
	fakeScript := `#!/bin/sh
printf '%s\n' "$@" > "$EXACTSIZE_COMPARE_TEST_LOG"
printf '\211PNG\r\n\032\nframe'
`
	if err := os.WriteFile(fakeFFmpeg, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	app := newApp(fakeFFmpeg, "", "compare-secret", fstest.MapFS{})
	app.job = job
	handler := app.routes()
	frame := func(path string, authenticated bool) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authenticated {
			req.Header.Set("X-ExactSize-Token", "compare-secret")
		}
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	if got := frame("/api/compare/frame/input?time=1.25", false); got.Code != http.StatusForbidden {
		t.Fatalf("frame extraction without auth returned %d, want 403", got.Code)
	}
	if got := frame("/api/compare/frame/input?time=invalid", true); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid frame timestamp returned %d, want 400", got.Code)
	}
	if got := frame("/api/compare/frame/elsewhere?time=1.25", true); got.Code != http.StatusNotFound {
		t.Fatalf("unknown frame side returned %d, want 404", got.Code)
	}
	preview := frame("/api/compare/frame/input?time=1.25", true)
	if preview.Code != http.StatusOK || !strings.HasPrefix(preview.Body.String(), "\x89PNG") {
		t.Fatalf("comparison frame = %d %q, want PNG response", preview.Code, preview.Body.String())
	}
	if got := preview.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("comparison frame Content-Type = %q, want image/png", got)
	}
	if got := preview.Header().Get("Content-Security-Policy"); !strings.Contains(got, "img-src 'self' data: blob:") {
		t.Fatalf("comparison response CSP does not permit generated frame images: %q", got)
	}
	args, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"1.250", input, "pipe:1"} {
		if !strings.Contains(string(args), expected) {
			t.Errorf("FFmpeg frame arguments are missing %q:\n%s", expected, args)
		}
	}

	job.request.Remux = true
	if got := frame("/api/compare/frame/input?time=1.25", true); got.Code != http.StatusConflict {
		t.Fatalf("remux frame comparison returned %d, want 409", got.Code)
	}
}

func TestRealCompareFrameWhenConfigured(t *testing.T) {
	input := os.Getenv("EXACTSIZE_REAL_COMPARE_INPUT")
	ffmpeg := os.Getenv("EXACTSIZE_REAL_COMPARE_FFMPEG")
	if input == "" || ffmpeg == "" {
		t.Skip("set EXACTSIZE_REAL_COMPARE_INPUT and EXACTSIZE_REAL_COMPARE_FFMPEG for the real-media integration check")
	}

	app := newApp(ffmpeg, "", "", fstest.MapFS{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame, err := app.extractCompareFrame(ctx, input, 35)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(frame)); err != nil {
		t.Fatalf("decode extracted comparison frame: %v", err)
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
