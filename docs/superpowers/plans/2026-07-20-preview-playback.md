# Comparison Playback Overhaul Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the stills-based compare viewer with two synced `<video>` players (audio from the compressed output, native lag-free seeking) plus an instant storyboard-sprite hover preview, per `docs/superpowers/specs/2026-07-20-preview-playback-design.md`.

**Architecture:** Direct playback of the real files whenever the browser can decode them; a one-time job-scoped FFmpeg conversion to a temp preview file only for unplayable sides (H.266, usually H.265, exotic inputs), with remux/audio-only fast paths. A storyboard JPEG sprite generated right after each successful encode makes timeline hovers a pure CSS `background-position` lookup. The per-frame PNG pipeline is deleted.

**Tech Stack:** Go 1.24 stdlib (`net/http`, `os/exec`, `crypto/subtle`), bundled FFmpeg (libx264/libvpx-vp9/aac/libopus verified present), vanilla JS/CSS. No new dependencies — `go.mod` must not change.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-20-preview-playback-design.md` — follow its tables verbatim (MIME mapping, planConversion rows, storyboard math).
- Repo path contains a space: always quote `"/run/media/bqj/8TB_Ext_HDD/Coding Projects/ExactSize"`.
- Comment style: full-sentence "why" comments above declarations, matching existing files. Table-driven tests.
- Audio is always the compressed output's track; if the output has no audio, the viewer stays silent and the mute button is disabled (title: "The compressed output has no audio track").
- Timeline ceiling everywhere: `max(0.1, min(inputDuration, outputDuration) - 0.05)`.
- Storyboard: `count = clamp(round(duration), 16, 180)`, `columns = 10`, `tileWidth = 192`, `tileHeight = round(192*h/w/2)*2`, `interval = duration/count`, JPEG `-q:v 4`.
- Convert quality: x264 `-preset veryfast -crf 18`; VP9 `-deadline realtime -cpu-used 5 -row-mt 1 -crf 24 -b:v 0`; both `-pix_fmt yuv420p`, scale cap `scale=w='min(1920,iw)':h='min(1920,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2`; audio transcode targets `aac 160k` (mp4) / `libopus 128k` (webm).
- Every FFmpeg invocation: `-hide_banner -nostdin -loglevel error`, map only `0:v:0` (+ `0:a:0?` where audio is kept), `-sn -dn`.
- Media GET auth: header token OR `?token=` query, both via `crypto/subtle.ConstantTimeCompare`.
- Temp dir naming must satisfy the existing sweeper: `exactsize-compare-<pid>-<rand>` under `os.TempDir()` (`comparePreviewPrefix` + `strconv.Itoa(os.Getpid())` + `-*`).
- The drag-and-drop locate improvements (recent-files pass + bounded recursive search in `server.go`/`server_test.go`) landed in a separate commit just before this plan executes. Do not modify `locateOriginalFile`, `dropSearchDirs`, `locateFileIn`, `searchDirTree`, `locateRecentFile`, `mountedMediaDirs`, `sanitizeDropName`, or their tests.
- Commits: one per task, message style matches history (short imperative), ending with:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_016A5au7UE6DUoz4CsqTKQ7p`.

## File Structure

- **Create `compare.go`** — everything compare-playback: `compareMime`, `planConversion`, `storyboardSpecFor`, the `compareAssets` manager (temp dir, storyboard generation, conversions, teardown), and all six HTTP handlers.
- **Create `compare_test.go`** — unit tests for the pure functions + endpoint tests (fake-ffmpeg pattern from `server_test.go:257-266`).
- **Modify `server.go`** — route table (remove `/api/compare/frame/{side}`, add six routes), `authMedia` wrapper, teardown hooks in `handleStartJob`/`cancelCurrentJob`, delete `handleCompareFrame`/`extractCompareFrame`.
- **Modify `server_test.go`** — delete `TestCompareFramesAreScopedAuthenticatedAndGenerated` and `TestRealCompareFrameWhenConfigured`; rewrite `TestCompletedCompressionOffersLargeSynchronizedComparison` as the playback contract test. `TestStaleComparePreviewCleanupPreservesActiveProcess` stays untouched.
- **Modify `web/index.html`** — stage `<img>`s → `<video>`s, toolbar play/mute buttons, hover `<img>` → background div.
- **Modify `web/app.js`** — new elements/state, open/convert/playback/sync/seek/hover logic; delete the frame-fetch machinery.
- **Modify `web/styles.css`** — `.compare-frame` covers `video`, button styles, `.compare-hover-thumb`.
- **Modify `main.go`** — `version = "1.9.0"`.
- **Modify `README.md`** — Visual comparison bullet rewrite.

Interface names used across tasks (defined in Tasks 1–4, consumed by 5–10):

```go
type sideMimes struct {
	Full       string `json:"fullMime"`
	Video      string `json:"videoMime"`
	Audio      string `json:"audioMime"`
	Optimistic bool   `json:"optimistic"`
}
func compareMime(info VideoInfo) sideMimes

type compareVerdicts struct{ Full, Video, Audio bool }   // JSON: full, video, audio
type compareProfiles struct{ H264MP4, VP9WebM bool }     // JSON: h264mp4, vp9webm
type convertPlan struct {
	VideoArgs []string // codec args, e.g. ["-c:v","copy"]
	AudioArgs []string // codec args or ["-an"]
	Filter    string   // "" or the scale cap expression
	Container string   // "mp4" | "webm"
}
func planConversion(info VideoInfo, v compareVerdicts, p compareProfiles) (convertPlan, error)

type storyboardSpec struct {
	Count, Columns, Rows, TileWidth, TileHeight int
	Interval                                    float64
}
func storyboardSpecFor(duration float64, width, height int) storyboardSpec

type storyboardState struct {
	State      string  `json:"state"` // pending|generating|ready|failed
	Interval   float64 `json:"interval"`
	Count      int     `json:"count"`
	Columns    int     `json:"columns"`
	TileWidth  int     `json:"tileWidth"`
	TileHeight int     `json:"tileHeight"`
}
type convertState struct {
	State    string  `json:"state"` // none|converting|ready|failed
	Progress float64 `json:"progress"`
	Error    string  `json:"error,omitempty"`
	path     string  // not serialized
}
type compareAssets struct { /* Task 4 */ }
func (a *App) ensureCompareAssets(job *Job) *compareAssets
func (a *App) teardownCompareAssets()
func compareTimelineDuration(input, output float64) float64
```

---

### Task 1: `compareMime` mapping

**Files:**
- Create: `compare.go`
- Create: `compare_test.go`

**Interfaces:**
- Consumes: `VideoInfo` (probe.go: fields `Path`, `VideoCodec`, `AudioCodec`, `PixelFormat`, `AudioTracks`).
- Produces: `sideMimes` + `compareMime(info VideoInfo) sideMimes` exactly as in File Structure.

- [ ] **Step 1: Write the failing test**

Create `compare_test.go`:

```go
package main

import "testing"

func TestCompareMime(t *testing.T) {
	cases := []struct {
		name string
		info VideoInfo
		want sideMimes
	}{
		{
			name: "h264 aac mp4 plays directly",
			info: VideoInfo{Path: "/v/a.mp4", VideoCodec: "h264", AudioCodec: "aac", AudioTracks: 1, PixelFormat: "yuv420p"},
			want: sideMimes{
				Full:  `video/mp4; codecs="avc1.64002A, mp4a.40.2"`,
				Video: `video/mp4; codecs="avc1.64002A"`,
				Audio: `audio/mp4; codecs="mp4a.40.2"`,
			},
		},
		{
			name: "mov is served as the mp4 family",
			info: VideoInfo{Path: "/v/a.MOV", VideoCodec: "h264", AudioTracks: 0},
			want: sideMimes{Full: `video/mp4; codecs="avc1.64002A"`, Video: `video/mp4; codecs="avc1.64002A"`},
		},
		{
			name: "mkv is optimistic regardless of codecs",
			info: VideoInfo{Path: "/v/a.mkv", VideoCodec: "hevc", AudioCodec: "opus", AudioTracks: 1},
			want: sideMimes{
				Full:       `video/x-matroska; codecs="hvc1.1.6.L123.B0, opus"`,
				Video:      `video/mp4; codecs="hvc1.1.6.L123.B0"`,
				Audio:      `audio/webm; codecs="opus"`,
				Optimistic: true,
			},
		},
		{
			name: "ten bit av1 in webm",
			info: VideoInfo{Path: "/v/a.webm", VideoCodec: "av1", PixelFormat: "yuv420p10le", AudioCodec: "vorbis", AudioTracks: 1},
			want: sideMimes{
				Full:  `video/webm; codecs="av01.0.08M.10, vorbis"`,
				Video: `video/mp4; codecs="av01.0.08M.10"`,
				Audio: `audio/webm; codecs="vorbis"`,
			},
		},
		{
			name: "vp9 targets webm and mp3 targets mp4",
			info: VideoInfo{Path: "/v/a.webm", VideoCodec: "vp9", AudioCodec: "mp3", AudioTracks: 1},
			want: sideMimes{
				Full:  `video/webm; codecs="vp09.00.40.08, mp3"`,
				Video: `video/webm; codecs="vp09.00.40.08"`,
				Audio: `audio/mp4; codecs="mp3"`,
			},
		},
		{
			name: "unknown container goes straight to convert",
			info: VideoInfo{Path: "/v/a.avi", VideoCodec: "mpeg4", AudioCodec: "pcm_s16le", AudioTracks: 1},
			want: sideMimes{},
		},
		{
			name: "vvc has no browser codec string",
			info: VideoInfo{Path: "/v/a.mp4", VideoCodec: "vvc", AudioCodec: "aac", AudioTracks: 1},
			want: sideMimes{Audio: `audio/mp4; codecs="mp4a.40.2"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareMime(tc.info); got != tc.want {
				t.Fatalf("compareMime(%+v)\n got %+v\nwant %+v", tc.info, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd "/run/media/bqj/8TB_Ext_HDD/Coding Projects/ExactSize" && go test -run TestCompareMime ./...`
Expected: FAIL — `undefined: sideMimes`, `undefined: compareMime`.

- [ ] **Step 3: Write the implementation**

Create `compare.go`:

```go
package main

import (
	"path/filepath"
	"strings"
)

// Comparison playback: sides play directly whenever the browser can decode
// them; only unplayable sides get a one-time converted preview. These helpers
// keep every codec decision server-side and table-testable.

// compareContainerMime maps a source file extension onto the MIME base used
// for the direct-play decision. MOV is ISO-BMFF family: Chromium demuxes it
// with its MP4 demuxer, while "video/quicktime" would always report
// unplayable. An empty result means "do not even try direct playback".
func compareContainerMime(path string) (mime string, optimistic bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov":
		return "video/mp4", false
	case ".webm":
		return "video/webm", false
	case ".mkv":
		// Chromium plays Matroska but canPlayType under-reports it, so the
		// client attempts direct playback regardless and relies on the
		// <video> error event to fall into the convert flow (Firefox).
		return "video/x-matroska", true
	default:
		return "", false
	}
}

// compareVideoCodecString maps a probed video codec onto a representative
// canPlayType string plus the container a converted preview would remux it
// into. Unknown codecs (H.266 today) return empty strings: no browser can
// decode them, so the side always converts.
func compareVideoCodecString(codec, pixelFormat string) (codecs, targetMime string) {
	switch mapProbeCodec(codec) {
	case "h264":
		return "avc1.64002A", "video/mp4"
	case "h265":
		return "hvc1.1.6.L123.B0", "video/mp4"
	case "av1":
		if strings.Contains(pixelFormat, "10le") {
			return "av01.0.08M.10", "video/mp4"
		}
		return "av01.0.08M.08", "video/mp4"
	case "vp9":
		return "vp09.00.40.08", "video/webm"
	default:
		return "", ""
	}
}

// compareAudioCodecString is the audio companion of compareVideoCodecString:
// the codec string plus the container a remuxed preview would carry it in.
func compareAudioCodecString(codec string) (codecs, targetMime string) {
	switch mapProbeAudioCodec(codec) {
	case "aac":
		return "mp4a.40.2", "audio/mp4"
	case "mp3":
		return "mp3", "audio/mp4"
	case "opus":
		return "opus", "audio/webm"
	case "vorbis":
		return "vorbis", "audio/webm"
	default:
		return "", ""
	}
}

// sideMimes carries the client's canPlayType inputs for one comparison side.
// Full is the source container + codecs (direct-play decision); Video and
// Audio express each codec in its conversion target container (convert
// verdicts). Optimistic marks Matroska sources, which are attempted directly
// no matter what canPlayType claims.
type sideMimes struct {
	Full       string `json:"fullMime"`
	Video      string `json:"videoMime"`
	Audio      string `json:"audioMime"`
	Optimistic bool   `json:"optimistic"`
}

func compareMime(info VideoInfo) sideMimes {
	container, optimistic := compareContainerMime(info.Path)
	videoCodec, videoTarget := compareVideoCodecString(info.VideoCodec, info.PixelFormat)
	var mimes sideMimes
	mimes.Optimistic = optimistic
	if videoCodec != "" {
		mimes.Video = videoTarget + `; codecs="` + videoCodec + `"`
	}
	audioCodec := ""
	if info.AudioTracks > 0 {
		var audioTarget string
		audioCodec, audioTarget = compareAudioCodecString(info.AudioCodec)
		if audioCodec != "" {
			mimes.Audio = audioTarget + `; codecs="` + audioCodec + `"`
		}
	}
	if container == "" || videoCodec == "" {
		return mimes
	}
	full := videoCodec
	if audioCodec != "" {
		full += ", " + audioCodec
	}
	mimes.Full = container + `; codecs="` + full + `"`
	return mimes
}
```

Note: `mapProbeCodec` lives in `encode.go:69`; `mapProbeAudioCodec` in `encode.go:491` (returns keys `aac`, `opus`, `vorbis`, `mp3` or `""`). Verify its exact keys before relying on it — if it maps differently (e.g. lowercase variants), adapt the switch to its actual outputs.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestCompareMime ./...`
Expected: PASS. Note: the mkv case expects `Full` to still be built from the source container even when the video codec has no MP4 string only if a string exists — for `hevc` it does (`hvc1.1.6.L123.B0`), so `Full` is populated. If the test's `vvc` case fails because `Full` is set: the `videoCodec == ""` guard covers it.

- [ ] **Step 5: Commit**

```bash
git add compare.go compare_test.go
git commit -m "Add comparison MIME mapping for direct-play decisions"
```

(Append the two trailer lines from Global Constraints to this and every commit.)

---

### Task 2: `planConversion` decision table

**Files:**
- Modify: `compare.go`
- Modify: `compare_test.go`

**Interfaces:**
- Consumes: `VideoInfo`, `mapProbeCodec`, `mapProbeAudioCodec`.
- Produces: `compareVerdicts`, `compareProfiles`, `convertPlan`, `planConversion(info, v, p) (convertPlan, error)`, and the constant `compareScaleFilter`.

- [ ] **Step 1: Write the failing test**

Append to `compare_test.go`:

```go
func TestPlanConversion(t *testing.T) {
	h264 := VideoInfo{VideoCodec: "h264", AudioCodec: "aac", AudioTracks: 1}
	hevcOpus := VideoInfo{VideoCodec: "hevc", AudioCodec: "opus", AudioTracks: 1}
	vp9Vorbis := VideoInfo{VideoCodec: "vp9", AudioCodec: "vorbis", AudioTracks: 1}
	vvc := VideoInfo{VideoCodec: "vvc", AudioCodec: "aac", AudioTracks: 1}
	silent := VideoInfo{VideoCodec: "vvc", AudioTracks: 0}
	exoticAudio := VideoInfo{VideoCodec: "h264", AudioCodec: "pcm_s16le", AudioTracks: 1}

	all := compareProfiles{H264MP4: true, VP9WebM: true}
	cases := []struct {
		name    string
		info    VideoInfo
		v       compareVerdicts
		p       compareProfiles
		want    convertPlan
		wantErr bool
	}{
		{
			name: "playable codecs in a foreign container remux to mp4",
			info: h264, v: compareVerdicts{Video: true, Audio: true}, p: all,
			want: convertPlan{VideoArgs: []string{"-c:v", "copy"}, AudioArgs: []string{"-c:a", "copy"}, Container: "mp4"},
		},
		{
			name: "vp9 with vorbis remuxes to webm",
			info: vp9Vorbis, v: compareVerdicts{Video: true, Audio: true}, p: all,
			want: convertPlan{VideoArgs: []string{"-c:v", "copy"}, AudioArgs: []string{"-c:a", "copy"}, Container: "webm"},
		},
		{
			name: "playable video with unplayable audio keeps the video untouched",
			info: exoticAudio, v: compareVerdicts{Video: true, Audio: false}, p: all,
			want: convertPlan{VideoArgs: []string{"-c:v", "copy"}, AudioArgs: []string{"-c:a", "aac", "-b:a", "160k"}, Container: "mp4"},
		},
		{
			name: "unplayable video transcodes to x264 and copies safe audio",
			info: hevcOpus, v: compareVerdicts{Video: false, Audio: true}, p: all,
			want: convertPlan{
				VideoArgs: []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p"},
				AudioArgs: []string{"-c:a", "copy"},
				Filter:    compareScaleFilter,
				Container: "mp4",
			},
		},
		{
			name: "no h264 support falls back to realtime vp9",
			info: vvc, v: compareVerdicts{Video: false, Audio: true}, p: compareProfiles{VP9WebM: true},
			want: convertPlan{
				VideoArgs: []string{"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "5", "-row-mt", "1", "-crf", "24", "-b:v", "0", "-pix_fmt", "yuv420p"},
				AudioArgs: []string{"-c:a", "libopus", "-b:a", "128k"},
				Filter:    compareScaleFilter,
				Container: "webm",
			},
		},
		{
			name: "silent sources drop audio entirely",
			info: silent, v: compareVerdicts{}, p: all,
			want: convertPlan{
				VideoArgs: []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p"},
				AudioArgs: []string{"-an"},
				Filter:    compareScaleFilter,
				Container: "mp4",
			},
		},
		{
			name: "no playable profile is an error",
			info: vvc, v: compareVerdicts{}, p: compareProfiles{},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planConversion(tc.info, tc.v, tc.p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("planConversion returned %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("planConversion\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}
```

Add `"reflect"` to the test file imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestPlanConversion ./...`
Expected: FAIL — `undefined: planConversion` (and friends).

- [ ] **Step 3: Write the implementation**

Append to `compare.go`:

```go
import "fmt" // merge into the existing import block

// compareScaleFilter caps converted previews at a 1920px longest edge with
// even dimensions, matching the aspect-preserving idiom used elsewhere.
const compareScaleFilter = "scale=w='min(1920,iw)':h='min(1920,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"

// compareVerdicts are the client's canPlayType results for one side's mimes;
// compareProfiles say which candidate preview profiles the browser can play.
type compareVerdicts struct {
	Full  bool `json:"full"`
	Video bool `json:"video"`
	Audio bool `json:"audio"`
}

type compareProfiles struct {
	H264MP4 bool `json:"h264mp4"`
	VP9WebM bool `json:"vp9webm"`
}

// convertPlan is a declarative FFmpeg strategy for one converted preview.
type convertPlan struct {
	VideoArgs []string
	AudioArgs []string
	Filter    string
	Container string
}

// mp4Video/webmVideo/mp4Audio/webmAudio say which codec copies are legal in
// each preview container.
var (
	mp4CopyVideo  = map[string]bool{"h264": true, "h265": true, "av1": true}
	webmCopyVideo = map[string]bool{"vp9": true, "av1": true}
	mp4CopyAudio  = map[string]bool{"aac": true, "mp3": true, "opus": true}
	webmCopyAudio = map[string]bool{"opus": true, "vorbis": true}
)

// planConversion picks the cheapest preview that the browser can play:
// container remux when both codecs decode, an audio-only transcode when just
// the audio is the problem, and a full video transcode otherwise.
func planConversion(info VideoInfo, v compareVerdicts, p compareProfiles) (convertPlan, error) {
	video := mapProbeCodec(info.VideoCodec)
	audio := ""
	if info.AudioTracks > 0 {
		audio = mapProbeAudioCodec(info.AudioCodec)
	}
	hasAudio := info.AudioTracks > 0

	if v.Video {
		// The browser decodes the video codec, so never re-encode it; pick
		// the container that legally carries the copy.
		if mp4CopyVideo[video] {
			if !hasAudio {
				return convertPlan{VideoArgs: []string{"-c:v", "copy"}, AudioArgs: []string{"-an"}, Container: "mp4"}, nil
			}
			if v.Audio && mp4CopyAudio[audio] {
				return convertPlan{VideoArgs: []string{"-c:v", "copy"}, AudioArgs: []string{"-c:a", "copy"}, Container: "mp4"}, nil
			}
			return convertPlan{VideoArgs: []string{"-c:v", "copy"}, AudioArgs: []string{"-c:a", "aac", "-b:a", "160k"}, Container: "mp4"}, nil
		}
		if webmCopyVideo[video] {
			if !hasAudio {
				return convertPlan{VideoArgs: []string{"-c:v", "copy"}, AudioArgs: []string{"-an"}, Container: "webm"}, nil
			}
			if v.Audio && webmCopyAudio[audio] {
				return convertPlan{VideoArgs: []string{"-c:v", "copy"}, AudioArgs: []string{"-c:a", "copy"}, Container: "webm"}, nil
			}
			return convertPlan{VideoArgs: []string{"-c:v", "copy"}, AudioArgs: []string{"-c:a", "libopus", "-b:a", "128k"}, Container: "webm"}, nil
		}
	}

	if p.H264MP4 {
		plan := convertPlan{
			VideoArgs: []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-pix_fmt", "yuv420p"},
			AudioArgs: []string{"-an"},
			Filter:    compareScaleFilter,
			Container: "mp4",
		}
		if hasAudio {
			if v.Audio && mp4CopyAudio[audio] {
				plan.AudioArgs = []string{"-c:a", "copy"}
			} else {
				plan.AudioArgs = []string{"-c:a", "aac", "-b:a", "160k"}
			}
		}
		return plan, nil
	}
	if p.VP9WebM {
		plan := convertPlan{
			VideoArgs: []string{"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "5", "-row-mt", "1", "-crf", "24", "-b:v", "0", "-pix_fmt", "yuv420p"},
			AudioArgs: []string{"-an"},
			Filter:    compareScaleFilter,
			Container: "webm",
		}
		if hasAudio {
			if v.Audio && webmCopyAudio[audio] {
				plan.AudioArgs = []string{"-c:a", "copy"}
			} else {
				plan.AudioArgs = []string{"-c:a", "libopus", "-b:a", "128k"}
			}
		}
		return plan, nil
	}
	return convertPlan{}, fmt.Errorf("this browser cannot play any preview format")
}
```

Note the spec nuance covered here: "hevc transcodes with audio copy" — opus copies legally into mp4 (`mp4CopyAudio` includes opus, both engines play Opus-in-MP4).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestPlanConversion ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add compare.go compare_test.go
git commit -m "Add converted-preview planner with remux fast paths"
```

---

### Task 3: Storyboard geometry

**Files:**
- Modify: `compare.go`
- Modify: `compare_test.go`

**Interfaces:**
- Produces: `storyboardSpec`, `storyboardSpecFor(duration float64, width, height int) storyboardSpec`, `storyboardState`, `compareTimelineDuration(input, output float64) float64`.

- [ ] **Step 1: Write the failing test**

Append to `compare_test.go`:

```go
func TestStoryboardSpecFor(t *testing.T) {
	cases := []struct {
		name              string
		duration          float64
		width, height     int
		count, rows, tileH int
	}{
		{name: "one thumb per second for mid-length clips", duration: 84.2, width: 1920, height: 1080, count: 84, rows: 9, tileH: 108},
		{name: "short clips keep a 16 thumb floor", duration: 3.4, width: 1920, height: 1080, count: 16, rows: 2, tileH: 108},
		{name: "long videos cap at 180 thumbs", duration: 4000, width: 1280, height: 720, count: 180, rows: 18, tileH: 108},
		{name: "portrait tiles stay 192 wide", duration: 30, width: 1080, height: 1920, count: 30, rows: 3, tileH: 342},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := storyboardSpecFor(tc.duration, tc.width, tc.height)
			if spec.Count != tc.count || spec.Rows != tc.rows || spec.TileHeight != tc.tileH ||
				spec.Columns != 10 || spec.TileWidth != 192 {
				t.Fatalf("storyboardSpecFor(%v, %d, %d) = %+v", tc.duration, tc.width, tc.height, spec)
			}
			wantInterval := tc.duration / float64(tc.count)
			if diff := spec.Interval - wantInterval; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("interval = %v, want %v", spec.Interval, wantInterval)
			}
		})
	}
}

func TestCompareTimelineDuration(t *testing.T) {
	if got := compareTimelineDuration(84.3, 84.25); got != 84.2 {
		t.Fatalf("timeline duration = %v, want 84.2", got)
	}
	if got := compareTimelineDuration(0.05, 9); got != 0.1 {
		t.Fatalf("tiny durations must clamp to 0.1, got %v", got)
	}
}
```

Portrait math check: `round(192*1920/1080/2)*2` = `round(341.33/2)*2` = `round(170.67)*2` = `171*2` = `342`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestStoryboardSpecFor|TestCompareTimelineDuration' ./...`
Expected: FAIL — `undefined: storyboardSpecFor`, `undefined: compareTimelineDuration`.

- [ ] **Step 3: Write the implementation**

Append to `compare.go` (add `"math"` to imports):

```go
// storyboardSpec fixes the sprite geometry for one output: Count evenly
// spaced tiles, TileWidth matching the 192px hover box, laid out in Columns
// columns. Interval is the seconds each tile represents.
type storyboardSpec struct {
	Count, Columns, Rows, TileWidth, TileHeight int
	Interval                                    float64
}

// storyboardSpecFor aims for roughly one thumbnail per second, floored at 16
// so short clips still scrub meaningfully and capped at 180 to bound sprite
// size (~10x192 x 18xTileHeight worst case).
func storyboardSpecFor(duration float64, width, height int) storyboardSpec {
	count := int(math.Round(duration))
	if count < 16 {
		count = 16
	}
	if count > 180 {
		count = 180
	}
	tileHeight := 108
	if width > 0 && height > 0 {
		tileHeight = int(math.Round(192*float64(height)/float64(width)/2)) * 2
	}
	return storyboardSpec{
		Count:      count,
		Columns:    10,
		Rows:       (count + 9) / 10,
		TileWidth:  192,
		TileHeight: tileHeight,
		Interval:   duration / float64(count),
	}
}

// storyboardState is the manifest shared with the client; a zero value means
// generation has not started.
type storyboardState struct {
	State      string  `json:"state"`
	Interval   float64 `json:"interval"`
	Count      int     `json:"count"`
	Columns    int     `json:"columns"`
	TileWidth  int     `json:"tileWidth"`
	TileHeight int     `json:"tileHeight"`
}

// compareTimelineDuration is the seekable ceiling: clamped just short of the
// shorter stream so a seek never lands past either side's end.
func compareTimelineDuration(input, output float64) float64 {
	duration := math.Min(input, output) - 0.05
	return math.Max(0.1, math.Round(duration*1000)/1000)
}
```

(The `Round(...*1000)/1000` keeps the JSON stable; the test's `84.3, 84.25` case → `84.2`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run 'TestStoryboardSpecFor|TestCompareTimelineDuration' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add compare.go compare_test.go
git commit -m "Add storyboard geometry and timeline ceiling helpers"
```

---

### Task 4: Compare assets manager, storyboard generation, teardown

**Files:**
- Modify: `compare.go`
- Modify: `compare_test.go`
- Modify: `server.go` (two hooks only: `handleStartJob`, `cancelCurrentJob`)

**Interfaces:**
- Consumes: `probeVideo` (probe.go), `progressSeconds` (encode.go:1632), `limitedBuffer` (encode.go), `comparePreviewPrefix` (server.go:42), Task 1-3 helpers.
- Produces:
  - `type compareAssets struct` with fields `job *Job`, `dir string`, `ctx context.Context`, `cancel context.CancelFunc`, `mu sync.Mutex`, `probes map[string]VideoInfo`, `story storyboardState`, `converts map[string]*convertState`.
  - `(a *App) ensureCompareAssets(job *Job) *compareAssets` — creates/reuses `App.compare` for this job and starts storyboard generation once.
  - `(a *App) teardownCompareAssets()` — cancel + delete dir.
  - `(c *compareAssets) sideProbe(ffprobe, side string) (VideoInfo, error)` — cached probe; side ∈ input|output.
  - `(c *compareAssets) ensureDir() (string, error)` — lazy `os.MkdirTemp(os.TempDir(), comparePreviewPrefix+strconv.Itoa(os.Getpid())+"-")`.
  - `type convertState struct{ State string; Progress float64; Error string; path string }` with JSON tags `state`, `progress`, `error,omitempty`.
  - `App.compare *compareAssets` field, guarded by `App.mu`.

- [ ] **Step 1: Write the failing test**

Append to `compare_test.go` (imports: `os`, `path/filepath`, `strings`, `time`, `context` as needed):

```go
// fakeCompareFFmpeg writes a script that logs its args and creates the last
// argument as a file, standing in for both storyboard and convert runs.
func fakeCompareFFmpeg(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/sh
log="$EXACTSIZE_COMPARE_TEST_LOG"
[ -n "$log" ] && printf '%s\n' "$@" >> "$log"
for last; do :; done
case "$last" in
  pipe:*) printf 'out_time_us=500000\nprogress=end\n' ;;
  *) printf 'jpegdata' > "$last" ;;
esac
`
	path := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func completedCompareJob(t *testing.T, dir string) *Job {
	t.Helper()
	input := filepath.Join(dir, "original.mp4")
	output := filepath.Join(dir, "compressed.mp4")
	for _, path := range []string{input, output} {
		if err := os.WriteFile(path, []byte("media-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	job := newJob(EncodeRequest{Input: input, Output: output})
	job.set(func(status *JobSnapshot) { status.State = "completed" })
	return job
}

func TestEnsureCompareAssetsGeneratesStoryboardOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	callLog := filepath.Join(dir, "calls")
	t.Setenv("EXACTSIZE_COMPARE_TEST_LOG", callLog)
	app := newApp(fakeCompareFFmpeg(t, dir), "", "secret", nil)
	job := completedCompareJob(t, dir)
	app.job = job

	assets := app.ensureCompareAssets(job)
	if assets == nil {
		t.Fatal("ensureCompareAssets returned nil for a completed job")
	}
	// Seed the probe cache so storyboard generation does not need ffprobe.
	assets.mu.Lock()
	assets.probes["output"] = VideoInfo{Path: job.request.Output, Duration: 30, Width: 1920, Height: 1080, VideoCodec: "h264"}
	assets.mu.Unlock()
	assets.generateStoryboard(app.ffmpeg)

	assets.mu.Lock()
	story := assets.story
	assets.mu.Unlock()
	if story.State != "ready" || story.Count != 30 || story.TileHeight != 108 {
		t.Fatalf("storyboard after generation = %+v", story)
	}
	sprite := filepath.Join(assets.dir, "storyboard.jpg")
	if _, err := os.Stat(sprite); err != nil {
		t.Fatalf("storyboard sprite missing: %v", err)
	}
	args, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"fps=30/30", "scale=192:-2", "tile=10x3", "-q:v", job.request.Output} {
		if !strings.Contains(string(args), expected) {
			t.Errorf("storyboard FFmpeg args missing %q:\n%s", expected, args)
		}
	}
	if again := app.ensureCompareAssets(job); again != assets {
		t.Fatal("ensureCompareAssets must reuse the job's assets")
	}
}

func TestTeardownCompareAssetsRemovesDirAndStopsWork(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	app := newApp(fakeCompareFFmpeg(t, dir), "", "secret", nil)
	job := completedCompareJob(t, dir)
	app.job = job
	assets := app.ensureCompareAssets(job)
	created, err := assets.ensureDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(created), comparePreviewPrefix) {
		t.Fatalf("preview dir %q must carry the sweeper prefix", created)
	}

	app.teardownCompareAssets()
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("teardown left the preview dir behind: %v", err)
	}
	if assets.ctx.Err() == nil {
		t.Fatal("teardown must cancel the assets context")
	}
	if app.compare != nil {
		t.Fatal("teardown must clear App.compare")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestEnsureCompareAssets|TestTeardownCompareAssets' ./...`
Expected: FAIL — `undefined` symbols.

- [ ] **Step 3: Write the implementation**

Append to `compare.go` (merge imports: `context`, `os`, `os/exec`, `path/filepath`, `strconv`, `sync`, `bufio`, `errors`):

```go
// convertState tracks one side's converted preview.
type convertState struct {
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Error    string  `json:"error,omitempty"`
	path     string
}

// compareAssets owns everything derived from one completed job: cached
// probes, the storyboard sprite, and converted previews. It lives until the
// next job starts or the app quits; cleanupStaleComparePreviews covers
// crashes.
type compareAssets struct {
	job    *Job
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	dir      string
	probes   map[string]VideoInfo
	story    storyboardState
	converts map[string]*convertState
}

// ensureCompareAssets returns the assets for job, creating them on first use.
// Callers hold no locks; App.mu guards the App.compare pointer.
func (a *App) ensureCompareAssets(job *Job) *compareAssets {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.compare != nil && a.compare.job == job {
		return a.compare
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.compare = &compareAssets{
		job:      job,
		ctx:      ctx,
		cancel:   cancel,
		probes:   map[string]VideoInfo{},
		story:    storyboardState{State: "pending"},
		converts: map[string]*convertState{"input": {State: "none"}, "output": {State: "none"}},
	}
	return a.compare
}

// prepareCompareAssets runs after job.run returns: eligible encodes get their
// storyboard generated immediately so hover previews are ready before the
// viewer opens.
func (a *App) prepareCompareAssets(job *Job) {
	snapshot := job.snapshot()
	if snapshot.State != "completed" || job.request.Remux || job.request.MuxAudio {
		return
	}
	assets := a.ensureCompareAssets(job)
	if _, err := assets.sideProbe(a.ffprobe, "output"); err != nil {
		return
	}
	assets.generateStoryboard(a.ffmpeg)
}

func (a *App) teardownCompareAssets() {
	a.mu.Lock()
	assets := a.compare
	a.compare = nil
	a.mu.Unlock()
	if assets == nil {
		return
	}
	assets.cancel()
	assets.mu.Lock()
	dir := assets.dir
	assets.mu.Unlock()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// sidePath maps a side name onto the real file it represents.
func (c *compareAssets) sidePath(side string) (string, bool) {
	switch side {
	case "input":
		return c.job.request.Input, true
	case "output":
		return c.job.request.Output, true
	default:
		return "", false
	}
}

// sideProbe caches ffprobe results per side; the viewer and every follow-up
// endpoint reuse them.
func (c *compareAssets) sideProbe(ffprobe, side string) (VideoInfo, error) {
	path, ok := c.sidePath(side)
	if !ok {
		return VideoInfo{}, errors.New("unknown comparison side")
	}
	c.mu.Lock()
	cached, ok := c.probes[side]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}
	info, err := probeVideo(c.ctx, ffprobe, path)
	if err != nil {
		return VideoInfo{}, err
	}
	c.mu.Lock()
	c.probes[side] = info
	c.mu.Unlock()
	return info, nil
}

// ensureDir lazily creates the job's preview directory using the PID-scoped
// prefix the stale sweeper understands.
func (c *compareAssets) ensureDir() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir != "" {
		return c.dir, nil
	}
	dir, err := os.MkdirTemp(os.TempDir(), comparePreviewPrefix+strconv.Itoa(os.Getpid())+"-")
	if err != nil {
		return "", err
	}
	c.dir = dir
	return dir, nil
}

// generateStoryboard renders the hover sprite from the compressed output in
// one FFmpeg pass. Failure is non-fatal: hovers fall back to time-only
// tooltips.
func (c *compareAssets) generateStoryboard(ffmpeg string) {
	c.mu.Lock()
	if c.story.State == "generating" || c.story.State == "ready" {
		c.mu.Unlock()
		return
	}
	info := c.probes["output"]
	if info.Duration <= 0 {
		// The probe cache is cold (no caller has probed the output yet);
		// leave the state pending so a later trigger can generate.
		c.mu.Unlock()
		return
	}
	spec := storyboardSpecFor(info.Duration, info.Width, info.Height)
	c.story = storyboardState{
		State:      "generating",
		Interval:   spec.Interval,
		Count:      spec.Count,
		Columns:    spec.Columns,
		TileWidth:  spec.TileWidth,
		TileHeight: spec.TileHeight,
	}
	c.mu.Unlock()

	fail := func() {
		c.mu.Lock()
		c.story.State = "failed"
		c.mu.Unlock()
	}
	dir, err := c.ensureDir()
	if err != nil {
		fail()
		return
	}
	sprite := filepath.Join(dir, "storyboard.jpg")
	filter := "fps=" + strconv.Itoa(spec.Count) + "/" + strconv.FormatFloat(info.Duration, 'f', -1, 64) +
		",scale=192:-2,tile=" + strconv.Itoa(spec.Columns) + "x" + strconv.Itoa(spec.Rows)
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", info.Path,
		"-map", "0:v:0", "-an", "-sn", "-dn",
		"-vf", filter, "-frames:v", "1", "-q:v", "4",
		"-f", "image2", "-y", sprite,
	}
	var stderr limitedBuffer
	command := exec.CommandContext(c.ctx, ffmpeg, args...)
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		fail()
		return
	}
	if stat, err := os.Stat(sprite); err != nil || stat.Size() == 0 {
		fail()
		return
	}
	c.mu.Lock()
	c.story.State = "ready"
	c.mu.Unlock()
}

// storyboardSnapshot returns a copy of the manifest for JSON responses.
func (c *compareAssets) storyboardSnapshot() storyboardState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.story
}
```

In `server.go`:

1. Add the field to `App` (after `uploads []string`): `compare *compareAssets`.
2. In `handleStartJob`, immediately before `job := newJob(request)` (inside the locked section is wrong — `teardownCompareAssets` takes `App.mu`; call it BEFORE `a.mu.Lock()`):

```go
	a.teardownCompareAssets()

	a.mu.Lock()
	if a.job != nil && !a.job.isTerminal() {
```

   (There is a small race if two start requests arrive together, but the second one 409s under the lock exactly as today; teardown of a job that then keeps running is impossible because a running job also 409s — however teardown before the running-check would still drop a *running* job's assets. Guard: completed-only teardown is unnecessary since compare assets only exist for completed jobs, and a running job cannot have them — `prepareCompareAssets` runs after `job.run` returns.)
3. Same function, replace `go job.run(a.ffmpeg, a.ffprobe)` with:

```go
	go func() {
		job.run(a.ffmpeg, a.ffprobe)
		a.prepareCompareAssets(job)
	}()
```

4. In `cancelCurrentJob`, after the uploads loop, add: `a.teardownCompareAssets()`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run 'TestEnsureCompareAssets|TestTeardownCompareAssets' ./...`
Expected: PASS. Also run `go vet ./...` — clean.

- [ ] **Step 5: Run the whole suite to catch regressions**

Run: `go test ./...`
Expected: PASS (the stills contract test still passes — nothing frontend changed yet).

- [ ] **Step 6: Commit**

```bash
git add compare.go compare_test.go server.go
git commit -m "Add job-scoped compare assets with storyboard generation"
```

---

### Task 5: `/api/compare/open` endpoint and media auth wrapper

**Files:**
- Modify: `compare.go`
- Modify: `compare_test.go`
- Modify: `server.go` (route table + `authMedia`)

**Interfaces:**
- Consumes: `currentComparisonJob` (server.go:372), `ensureCompareAssets`, `sideProbe`, `compareMime`, `compareTimelineDuration`, `storyboardSnapshot`, `writeJSON`/`writeError`, `decodeJSON`.
- Produces:
  - `(a *App) handleCompareOpen(w, r)` on `POST /api/compare/open`.
  - `(a *App) authMedia(next http.HandlerFunc) http.HandlerFunc` — header token OR `?token=`, `subtle.ConstantTimeCompare` (used by Tasks 6).
  - `type compareSideInfo struct { sideMimes; HasAudio bool; Width, Height int; Duration float64 }` (JSON: `hasAudio`, `width`, `height`, `duration`; embedded `sideMimes` fields flatten).
  - `type compareOpenResponse struct { Duration float64; Sides map[string]compareSideInfo; Storyboard storyboardState; Previews map[string]convertState }` (JSON: `duration`, `sides`, `storyboard`, `previews`).
  - `(c *compareAssets) convertSnapshot(side string) convertState`.
  - Test helper `fakeCompareFFprobe(t, dir) string` (used by Task 7 too).

- [ ] **Step 1: Write the failing test**

Append to `compare_test.go`:

```go
// fakeCompareFFprobe answers every probe with a fixed h264+aac 1080p30 file.
func fakeCompareFFprobe(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/sh
cat <<'JSON'
{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"avg_frame_rate":"30/1","pix_fmt":"yuv420p"},{"codec_type":"audio","codec_name":"aac","channels":2,"sample_rate":"48000"}],"format":{"duration":"30.000000","size":"1000","format_name":"mov,mp4,m4a,3gp,3g2,mj2"}}
JSON
`
	path := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompareOpenReportsSidesStoryboardAndPreviews(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	app := newApp(fakeCompareFFmpeg(t, dir), fakeCompareFFprobe(t, dir), "secret", fstest.MapFS{})
	job := completedCompareJob(t, dir)
	app.job = job
	t.Cleanup(app.teardownCompareAssets)
	handler := app.routes()

	call := func(authenticated bool) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/compare/open", strings.NewReader("{}"))
		if authenticated {
			req.Header.Set("X-ExactSize-Token", "secret")
		}
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	if got := call(false); got.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated open returned %d, want 403", got.Code)
	}
	response := call(true)
	if response.Code != http.StatusOK {
		t.Fatalf("open returned %d: %s", response.Code, response.Body.String())
	}
	var payload compareOpenResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Duration != 29.95 {
		t.Fatalf("timeline duration = %v, want 29.95", payload.Duration)
	}
	output, ok := payload.Sides["output"]
	if !ok || !strings.Contains(output.Full, "avc1.64002A") || !output.HasAudio || output.Width != 1920 {
		t.Fatalf("output side = %+v", output)
	}
	if payload.Previews["input"].State != "none" || payload.Previews["output"].State != "none" {
		t.Fatalf("previews = %+v", payload.Previews)
	}
	if payload.Storyboard.State == "" {
		t.Fatalf("storyboard manifest missing: %+v", payload.Storyboard)
	}

	job.request.Remux = true
	if got := call(true); got.Code != http.StatusConflict {
		t.Fatalf("remux open returned %d, want 409", got.Code)
	}
	job.request.Remux = false
}
```

Add `"encoding/json"`, `"net/http"`, `"net/http/httptest"`, `"testing/fstest"` to the test imports as needed.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestCompareOpenReports ./...`
Expected: FAIL — `undefined: compareOpenResponse`; route missing (404) once types exist.

- [ ] **Step 3: Write the implementation**

Append to `compare.go` (merge `net/http` into imports):

```go
// compareSideInfo is one side's playability facts for the viewer: the
// canPlayType inputs plus the metadata the player needs before loadedmetadata.
type compareSideInfo struct {
	sideMimes
	HasAudio bool    `json:"hasAudio"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Duration float64 `json:"duration"`
}

type compareOpenResponse struct {
	Duration   float64                    `json:"duration"`
	Sides      map[string]compareSideInfo `json:"sides"`
	Storyboard storyboardState            `json:"storyboard"`
	Previews   map[string]convertState    `json:"previews"`
}

// convertSnapshot copies one side's conversion state for JSON responses.
func (c *compareAssets) convertSnapshot(side string) convertState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if convert := c.converts[side]; convert != nil {
		return *convert
	}
	return convertState{State: "none"}
}

// handleCompareOpen is the viewer's single bootstrap call: cached probes for
// both sides, the storyboard manifest, and any converted previews that
// already exist, so a reopened viewer is instant.
func (a *App) handleCompareOpen(w http.ResponseWriter, r *http.Request) {
	job, status, message := a.currentComparisonJob()
	if job == nil {
		writeError(w, status, message)
		return
	}
	assets := a.ensureCompareAssets(job)
	sides := map[string]compareSideInfo{}
	for _, side := range []string{"input", "output"} {
		info, err := assets.sideProbe(a.ffprobe, side)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		sides[side] = compareSideInfo{
			sideMimes: compareMime(info),
			HasAudio:  info.AudioTracks > 0,
			Width:     info.Width,
			Height:    info.Height,
			Duration:  info.Duration,
		}
	}
	// The storyboard normally starts right after the encode; opening the
	// viewer is the fallback trigger (a restart or an early open).
	go assets.generateStoryboard(a.ffmpeg)
	writeJSON(w, http.StatusOK, compareOpenResponse{
		Duration:   compareTimelineDuration(sides["input"].Duration, sides["output"].Duration),
		Sides:      sides,
		Storyboard: assets.storyboardSnapshot(),
		Previews: map[string]convertState{
			"input":  assets.convertSnapshot("input"),
			"output": assets.convertSnapshot("output"),
		},
	})
}
```

`generateStoryboard` already guards a cold probe cache (Task 4: `info.Duration <= 0` returns with the state left `pending`); `handleCompareOpen` probes both sides before the goroutine starts, so on this path the cache is warm and generation proceeds. The `pending` initial state (set in `ensureCompareAssets`) is what makes this test's `Storyboard.State` assertion deterministic despite the async goroutine.

In `server.go`:

1. Add to `routes()` after the current-job routes:

```go
	mux.HandleFunc("POST /api/compare/open", a.auth(a.handleCompareOpen))
```

2. Add the media auth wrapper next to `auth` (import `crypto/subtle`):

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestCompareOpenReports ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add compare.go compare_test.go server.go
git commit -m "Add comparison open endpoint and media token auth"
```

---

### Task 6: Media and storyboard serving

**Files:**
- Modify: `compare.go`
- Modify: `compare_test.go`
- Modify: `server.go` (routes)

**Interfaces:**
- Consumes: `authMedia` (Task 5), `sidePath`, `convertSnapshot`, `storyboardSnapshot`, `ensureDir`.
- Produces:
  - `(a *App) handleCompareMedia` on `GET /api/compare/media/{side}` (`?variant=source|preview`).
  - `(a *App) handleCompareStoryboard` on `GET /api/compare/storyboard`.
  - `(a *App) handleCompareStoryboardManifest` on `GET /api/compare/storyboard/manifest`.
  - `compareMediaContentType(path string) string`.

- [ ] **Step 1: Write the failing test**

Append to `compare_test.go`:

```go
func TestCompareMediaServesRangesWithQueryToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	app := newApp(fakeCompareFFmpeg(t, dir), fakeCompareFFprobe(t, dir), "secret", fstest.MapFS{})
	job := completedCompareJob(t, dir)
	app.job = job
	t.Cleanup(app.teardownCompareAssets)
	handler := app.routes()

	get := func(path string, header http.Header) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for key, values := range header {
			req.Header[key] = values
		}
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	if got := get("/api/compare/media/output?variant=source", nil); got.Code != http.StatusForbidden {
		t.Fatalf("media without token returned %d, want 403", got.Code)
	}
	full := get("/api/compare/media/output?variant=source&token=secret", nil)
	if full.Code != http.StatusOK || full.Body.String() != "media-bytes" {
		t.Fatalf("media = %d %q", full.Code, full.Body.String())
	}
	if got := full.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("media Content-Type = %q, want video/mp4", got)
	}
	if got := full.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("media Accept-Ranges = %q, want bytes", got)
	}
	ranged := get("/api/compare/media/output?variant=source&token=secret",
		http.Header{"Range": []string{"bytes=2-5"}})
	if ranged.Code != http.StatusPartialContent || ranged.Body.String() != "dia-" {
		t.Fatalf("ranged media = %d %q, want 206 \"dia-\"", ranged.Code, ranged.Body.String())
	}
	if got := get("/api/compare/media/output?variant=preview&token=secret", nil); got.Code != http.StatusNotFound {
		t.Fatalf("missing preview returned %d, want 404", got.Code)
	}
	if got := get("/api/compare/media/elsewhere?variant=source&token=secret", nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown side returned %d, want 404", got.Code)
	}

	if got := get("/api/compare/storyboard?token=secret", nil); got.Code != http.StatusNotFound {
		t.Fatalf("storyboard before generation returned %d, want 404", got.Code)
	}
	assets := app.ensureCompareAssets(job)
	previewDir, err := assets.ensureDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previewDir, "storyboard.jpg"), []byte("jpegdata"), 0o600); err != nil {
		t.Fatal(err)
	}
	assets.mu.Lock()
	assets.story = storyboardState{State: "ready", Interval: 1, Count: 30, Columns: 10, TileWidth: 192, TileHeight: 108}
	assets.mu.Unlock()
	sprite := get("/api/compare/storyboard?token=secret", nil)
	if sprite.Code != http.StatusOK || sprite.Body.String() != "jpegdata" {
		t.Fatalf("storyboard = %d %q", sprite.Code, sprite.Body.String())
	}
	if got := sprite.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("storyboard Content-Type = %q", got)
	}
	manifest := get("/api/compare/storyboard/manifest", http.Header{"X-Exactsize-Token": []string{"secret"}})
	if manifest.Code != http.StatusOK || !strings.Contains(manifest.Body.String(), `"ready"`) {
		t.Fatalf("manifest = %d %q", manifest.Code, manifest.Body.String())
	}

	job.request.Remux = true
	if got := get("/api/compare/media/output?variant=source&token=secret", nil); got.Code != http.StatusConflict {
		t.Fatalf("remux media returned %d, want 409", got.Code)
	}
	job.request.Remux = false
}
```

(Header key note: `httptest.NewRequest` canonicalizes `X-Exactsize-Token` to `X-Exactsize-Token`; Go canonicalization of `X-ExactSize-Token` is `X-Exactsize-Token`, and `Header.Get` is case-insensitive, so both spellings work with `req.Header.Set`. If setting via the map literal fails the auth check, switch to `req.Header.Set("X-ExactSize-Token", "secret")`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestCompareMediaServes ./...`
Expected: FAIL — 404s everywhere (routes missing).

- [ ] **Step 3: Write the implementation**

Append to `compare.go` (merge `time` NOT needed; `net/http`, `os`, `strings`, `filepath` already):

```go
// compareMediaContentType maps a media file onto the Content-Type served to
// the <video> element; http.ServeContent only sniffs when nothing is set.
func compareMediaContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	default:
		return "application/octet-stream"
	}
}

// handleCompareMedia serves the real input/output files (variant=source) or a
// converted preview (variant=preview) with Range support, which is what gives
// the viewer native, lag-free seeking.
func (a *App) handleCompareMedia(w http.ResponseWriter, r *http.Request) {
	job, status, message := a.currentComparisonJob()
	if job == nil {
		writeError(w, status, message)
		return
	}
	assets := a.ensureCompareAssets(job)
	side := r.PathValue("side")
	path, ok := assets.sidePath(side)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown comparison side")
		return
	}
	if r.URL.Query().Get("variant") == "preview" {
		convert := assets.convertSnapshot(side)
		if convert.State != "ready" || convert.path == "" {
			writeError(w, http.StatusNotFound, "no converted preview is ready for this side")
			return
		}
		path = convert.path
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "the comparison file no longer exists")
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", compareMediaContentType(path))
	http.ServeContent(w, r, "", stat.ModTime(), file)
}

// handleCompareStoryboard serves the hover sprite once it is ready.
func (a *App) handleCompareStoryboard(w http.ResponseWriter, r *http.Request) {
	job, status, message := a.currentComparisonJob()
	if job == nil {
		writeError(w, status, message)
		return
	}
	assets := a.ensureCompareAssets(job)
	assets.mu.Lock()
	ready := assets.story.State == "ready"
	sprite := filepath.Join(assets.dir, "storyboard.jpg")
	hasDir := assets.dir != ""
	assets.mu.Unlock()
	if !ready || !hasDir {
		writeError(w, http.StatusNotFound, "the hover storyboard is not ready yet")
		return
	}
	file, err := os.Open(sprite)
	if err != nil {
		writeError(w, http.StatusNotFound, "the hover storyboard is not ready yet")
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, "", stat.ModTime(), file)
}

// handleCompareStoryboardManifest reports storyboard geometry and readiness;
// the client polls it while generation runs.
func (a *App) handleCompareStoryboardManifest(w http.ResponseWriter, _ *http.Request) {
	job, status, message := a.currentComparisonJob()
	if job == nil {
		writeError(w, status, message)
		return
	}
	writeJSON(w, http.StatusOK, a.ensureCompareAssets(job).storyboardSnapshot())
}
```

Note `convertState.path` is read through `convertSnapshot` (returns the struct copy including the unexported field — same package, fine).

In `server.go` `routes()`, add:

```go
	mux.HandleFunc("GET /api/compare/media/{side}", a.authMedia(a.handleCompareMedia))
	mux.HandleFunc("GET /api/compare/storyboard", a.authMedia(a.handleCompareStoryboard))
	mux.HandleFunc("GET /api/compare/storyboard/manifest", a.auth(a.handleCompareStoryboardManifest))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestCompareMediaServes ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add compare.go compare_test.go server.go
git commit -m "Serve comparison media with Range support and the hover storyboard"
```

---

### Task 7: Conversion endpoints

**Files:**
- Modify: `compare.go`
- Modify: `compare_test.go`
- Modify: `server.go` (routes)

**Interfaces:**
- Consumes: `planConversion`, `ensureDir`, `sideProbe`, `progressSeconds` (encode.go — signature `progressSeconds(key, value string, fps float64) (float64, bool)`; pass `0` fps so only time-based keys report), `muxerName` (encode.go:1568 — verify it returns valid `-f` names for "mp4" and "webm"), `limitedBuffer` (encode.go).
- Produces:
  - `(c *compareAssets) startConvert(ffmpeg, side string, info VideoInfo, v compareVerdicts, p compareProfiles) convertState`.
  - `(c *compareAssets) runConvert(ffmpeg, side string, info VideoInfo, plan convertPlan)` (goroutine body).
  - `(a *App) handleCompareConvertStart` on `POST /api/compare/convert`.
  - `(a *App) handleCompareConvertStatus` on `GET /api/compare/convert/{side}`.

- [ ] **Step 1: Write the failing test**

Append to `compare_test.go` (add `"time"`, `"bytes"` if missing):

```go
func TestCompareConvertLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	callLog := filepath.Join(dir, "convert-calls")
	t.Setenv("EXACTSIZE_COMPARE_TEST_LOG", callLog)
	app := newApp(fakeCompareFFmpeg(t, dir), fakeCompareFFprobe(t, dir), "secret", fstest.MapFS{})
	job := completedCompareJob(t, dir)
	app.job = job
	t.Cleanup(app.teardownCompareAssets)
	handler := app.routes()

	request := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-ExactSize-Token", "secret")
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	// An unplayable side converts; the fake ffmpeg finishes instantly.
	start := request(http.MethodPost, "/api/compare/convert",
		`{"side":"output","verdicts":{"full":false,"video":false,"audio":true},"profiles":{"h264mp4":true,"vp9webm":true}}`)
	if start.Code != http.StatusAccepted {
		t.Fatalf("convert start returned %d: %s", start.Code, start.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	var status convertState
	for {
		response := request(http.MethodGet, "/api/compare/convert/output", "")
		if response.Code != http.StatusOK {
			t.Fatalf("convert status returned %d", response.Code)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.State == "ready" || status.State == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversion never finished: %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.State != "ready" || status.Progress != 1 {
		t.Fatalf("conversion state = %+v, want ready/1", status)
	}
	args, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"libx264", "-crf", "18", "-movflags", "+faststart", "preview-output.mp4", "-progress"} {
		if !strings.Contains(string(args), expected) {
			t.Errorf("convert FFmpeg args missing %q:\n%s", expected, args)
		}
	}
	preview := request(http.MethodGet, "/api/compare/media/output?variant=preview", "")
	if preview.Code != http.StatusOK || preview.Body.Len() == 0 {
		t.Fatalf("converted preview = %d %q", preview.Code, preview.Body.String())
	}
	if got := preview.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("converted preview Content-Type = %q", got)
	}

	// Starting again is idempotent: the ready state comes straight back.
	again := request(http.MethodPost, "/api/compare/convert",
		`{"side":"output","verdicts":{},"profiles":{}}`)
	if again.Code != http.StatusAccepted || !strings.Contains(again.Body.String(), `"ready"`) {
		t.Fatalf("repeat convert = %d %s", again.Code, again.Body.String())
	}

	// A browser with no playable profile fails fast with a clear error.
	failed := request(http.MethodPost, "/api/compare/convert",
		`{"side":"input","verdicts":{},"profiles":{}}`)
	if failed.Code != http.StatusAccepted || !strings.Contains(failed.Body.String(), `"failed"`) {
		t.Fatalf("unplayable convert = %d %s", failed.Code, failed.Body.String())
	}

	if got := request(http.MethodPost, "/api/compare/convert", `{"side":"elsewhere"}`); got.Code != http.StatusNotFound {
		t.Fatalf("unknown convert side returned %d, want 404", got.Code)
	}
}
```

The fake ffmpeg from Task 4 creates the last argument as a file, so `preview-output.mp4` materializes and the run exits 0 with no progress lines — the final state jumps to ready/1 without intermediate progress, which is all this test asserts.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestCompareConvertLifecycle ./...`
Expected: FAIL — 404 (routes missing).

- [ ] **Step 3: Write the implementation**

Append to `compare.go` (merge `bufio`, `math`, `os/exec` imports):

```go
// startConvert begins (or reports) one side's preview conversion. It is
// idempotent so reopening the viewer resumes polling instead of re-encoding.
func (c *compareAssets) startConvert(ffmpeg, side string, info VideoInfo, v compareVerdicts, p compareProfiles) convertState {
	c.mu.Lock()
	convert := c.converts[side]
	if convert == nil {
		convert = &convertState{State: "none"}
		c.converts[side] = convert
	}
	if convert.State == "converting" || convert.State == "ready" {
		snapshot := *convert
		c.mu.Unlock()
		return snapshot
	}
	plan, err := planConversion(info, v, p)
	if err != nil {
		convert.State = "failed"
		convert.Error = err.Error()
		snapshot := *convert
		c.mu.Unlock()
		return snapshot
	}
	convert.State = "converting"
	convert.Progress = 0
	convert.Error = ""
	snapshot := *convert
	c.mu.Unlock()
	go c.runConvert(ffmpeg, side, info, plan)
	return snapshot
}

// runConvert executes the planned FFmpeg command, reporting progress from the
// -progress stream. The output goes to the job's preview directory, so the
// media endpoint can serve it the moment the state flips to ready.
func (c *compareAssets) runConvert(ffmpeg, side string, info VideoInfo, plan convertPlan) {
	fail := func(detail string) {
		c.mu.Lock()
		convert := c.converts[side]
		convert.State = "failed"
		convert.Error = detail
		c.mu.Unlock()
	}
	dir, err := c.ensureDir()
	if err != nil {
		fail(err.Error())
		return
	}
	output := filepath.Join(dir, "preview-"+side+"."+plan.Container)
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-i", info.Path, "-map", "0:v:0"}
	dropAudio := len(plan.AudioArgs) == 1 && plan.AudioArgs[0] == "-an"
	if !dropAudio {
		args = append(args, "-map", "0:a:0?")
	}
	args = append(args, "-sn", "-dn")
	if plan.Filter != "" {
		args = append(args, "-vf", plan.Filter)
	}
	args = append(args, plan.VideoArgs...)
	args = append(args, plan.AudioArgs...)
	if plan.Container == "mp4" {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, "-progress", "pipe:1", "-nostats", "-f", muxerName(plan.Container), "-y", output)

	command := exec.CommandContext(c.ctx, ffmpeg, args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		fail(err.Error())
		return
	}
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		fail(err.Error())
		return
	}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), "=")
		if !found {
			continue
		}
		if seconds, ok := progressSeconds(key, value, 0); ok && info.Duration > 0 {
			progress := math.Min(1, seconds/info.Duration)
			c.mu.Lock()
			c.converts[side].Progress = progress
			c.mu.Unlock()
		}
	}
	if err := command.Wait(); err != nil {
		if c.ctx.Err() != nil {
			return
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		fail(detail)
		return
	}
	c.mu.Lock()
	convert := c.converts[side]
	convert.State = "ready"
	convert.Progress = 1
	convert.path = output
	c.mu.Unlock()
}

// handleCompareConvertStart validates the request and kicks the conversion.
func (a *App) handleCompareConvertStart(w http.ResponseWriter, r *http.Request) {
	job, status, message := a.currentComparisonJob()
	if job == nil {
		writeError(w, status, message)
		return
	}
	var request struct {
		Side     string          `json:"side"`
		Verdicts compareVerdicts `json:"verdicts"`
		Profiles compareProfiles `json:"profiles"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	assets := a.ensureCompareAssets(job)
	if _, ok := assets.sidePath(request.Side); !ok {
		writeError(w, http.StatusNotFound, "unknown comparison side")
		return
	}
	info, err := assets.sideProbe(a.ffprobe, request.Side)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, assets.startConvert(a.ffmpeg, request.Side, info, request.Verdicts, request.Profiles))
}

// handleCompareConvertStatus reports one side's conversion progress.
func (a *App) handleCompareConvertStatus(w http.ResponseWriter, r *http.Request) {
	job, status, message := a.currentComparisonJob()
	if job == nil {
		writeError(w, status, message)
		return
	}
	assets := a.ensureCompareAssets(job)
	side := r.PathValue("side")
	if _, ok := assets.sidePath(side); !ok {
		writeError(w, http.StatusNotFound, "unknown comparison side")
		return
	}
	writeJSON(w, http.StatusOK, assets.convertSnapshot(side))
}
```

In `server.go` `routes()`, add:

```go
	mux.HandleFunc("POST /api/compare/convert", a.auth(a.handleCompareConvertStart))
	mux.HandleFunc("GET /api/compare/convert/{side}", a.auth(a.handleCompareConvertStatus))
```

Check: `convertState` JSON must round-trip for the test's `json.Unmarshal` — `State`, `Progress`, `Error` are exported with tags; `path` stays internal. ✓

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestCompareConvertLifecycle ./...`
Expected: PASS. Then `go test ./...` — everything green, `go vet ./...` clean.

- [ ] **Step 5: Commit**

```bash
git add compare.go compare_test.go server.go
git commit -m "Add preview conversion endpoints with progress reporting"
```

---

### Task 8: Delete the per-frame PNG pipeline (server side)

**Files:**
- Modify: `server.go`
- Modify: `server_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: absence — `/api/compare/frame/{side}`, `handleCompareFrame`, `extractCompareFrame` no longer exist. (The frontend still references the URL until Task 9; that string lives in `web/app.js`, which no Go code calls, so the suite stays green.)

- [ ] **Step 1: Delete the endpoint**

In `server.go`:
1. Remove the route line: `mux.HandleFunc("GET /api/compare/frame/{side}", a.auth(a.handleCompareFrame))`.
2. Delete `handleCompareFrame` (server.go:386-427) and `extractCompareFrame` (server.go:429-456), including their doc comments.
3. Fix imports: `bytes` and `math` were used by the deleted functions — remove each only if `go build` reports it unused (check other uses first).

In `server_test.go`:
1. Delete `TestCompareFramesAreScopedAuthenticatedAndGenerated` (lines ~241-313) and `TestRealCompareFrameWhenConfigured` (~315-332).
2. Remove now-unused imports (`image/png`, possibly `bytes`, `context`, `time` — let the compiler decide).

- [ ] **Step 2: Verify the suite is green**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS — the stills contract test only greps `web/` files, which are untouched.

- [ ] **Step 3: Commit**

```bash
git add server.go server_test.go
git commit -m "Remove the per-frame comparison extraction endpoint"
```

---

### Task 9: Frontend overhaul — playback, sync, hover sprite

**Files:**
- Modify: `server_test.go` (rewrite `TestCompletedCompressionOffersLargeSynchronizedComparison` — the contract test drives this task)
- Modify: `web/index.html`
- Modify: `web/styles.css`
- Modify: `web/app.js`

**Interfaces:**
- Consumes: every endpoint from Tasks 5-7; existing app.js helpers `api(path, options)` (returns parsed JSON, throws `Error(message)`, sets the token header — near line 155), `formatDuration(seconds)`, `token` (line 3), `showToast`.
- Produces: the element ids `compareOriginalVideo`, `compareCompressedVideo`, `comparePlay`, `compareMute`, `compareHoverThumb`, `compareHoverStage` and the JS functions named below (the contract test pins them).

- [ ] **Step 1: Rewrite the contract test (it must FAIL now)**

Replace the entire body of `TestCompletedCompressionOffersLargeSynchronizedComparison` in `server_test.go` with:

```go
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
```

Also delete the now-dead sections that the old test carried below this point if they referenced removed ids (`compareHoverOriginal`, `compareHoverCompressed`, `compare-hover-divider` checks can stay if desired — they assert absence and still pass; simplest is to keep the function exactly as above and delete the remainder of the old body).

- [ ] **Step 2: Run the contract test to verify it fails**

Run: `go test -run TestCompletedCompressionOffersLargeSynchronizedComparison ./...`
Expected: FAIL — markup missing `compareOriginalVideo`.

- [ ] **Step 3: Update `web/index.html`**

Replace the stage images (lines 299-300) with:

```html
        <video class="compare-frame compare-original" id="compareOriginalVideo" muted playsinline preload="auto"></video>
        <video class="compare-frame compare-compressed" id="compareCompressedVideo" playsinline preload="auto"></video>
```

Replace the footer (lines 308-320) with:

```html
      <footer class="compare-controls" id="compareControls">
        <button class="icon-button compare-play" id="comparePlay" type="button" aria-label="Play or pause" title="Play or pause" disabled>
          <svg class="play-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M8 5l11 7-11 7V5z"/></svg>
          <svg class="pause-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M7 5h4v14H7zM13 5h4v14h-4z"/></svg>
        </button>
        <button class="icon-button compare-mute" id="compareMute" type="button" aria-label="Mute or unmute" title="Mute audio" disabled>
          <svg class="sound-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 9v6h4l5 4V5L8 9H4z"/><path d="M16.5 8.5a5 5 0 010 7"/></svg>
          <svg class="muted-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 9v6h4l5 4V5L8 9H4z"/><path d="M16 9l5 6M21 9l-5 6"/></svg>
        </button>
        <div class="compare-timeline-wrap" id="compareTimelineWrap">
          <div class="compare-hover-preview" id="compareHoverPreview" aria-hidden="true" hidden>
            <div class="compare-hover-stage" id="compareHoverStage">
              <div class="compare-hover-thumb" id="compareHoverThumb"></div>
            </div>
            <output class="compare-hover-time" id="compareHoverTime">00:00</output>
          </div>
          <input class="compare-timeline" id="compareTimeline" type="range" min="0" max="1000" step="1" value="0" aria-label="Playback position" disabled>
        </div>
        <output class="compare-time" id="compareTime" for="compareTimeline">00:00 / 00:00</output>
        <span class="compare-hint" id="compareHint">Play or scrub the timeline · hover for an instant preview.</span>
      </footer>
```

- [ ] **Step 4: Update `web/styles.css`**

1. In `.compare-controls` (line ~1258), change `grid-template-columns: minmax(120px, 1fr) auto auto;` to `grid-template-columns: auto auto minmax(120px, 1fr) auto auto;`.
2. Replace the `.compare-hover-frame` rule (lines ~1298-1305) with:

```css
.compare-hover-thumb {
  width: 100%;
  height: 100%;
  background-color: #050505;
  background-repeat: no-repeat;
}
```

3. After the `.compare-hover-time` rule, add:

```css
.compare-play .pause-icon,
.compare-mute .muted-icon {
  display: none;
}

.compare-play.playing .play-icon,
.compare-mute.muted .sound-icon {
  display: none;
}

.compare-play.playing .pause-icon,
.compare-mute.muted .muted-icon {
  display: block;
}

.compare-play[disabled],
.compare-mute[disabled] {
  opacity: .45;
  cursor: default;
}
```

- [ ] **Step 5: Update `web/app.js` — elements and state**

In the `elements` map: remove `compareOriginalFrame`, `compareCompressedFrame`, `compareHoverFrame`; add:

```js
  compareOriginalVideo: $("compareOriginalVideo"),
  compareCompressedVideo: $("compareCompressedVideo"),
  comparePlay: $("comparePlay"),
  compareMute: $("compareMute"),
  compareHoverStage: $("compareHoverStage"),
  compareHoverThumb: $("compareHoverThumb"),
```

In `state`: remove the eleven `compareDuration`/`compareLoadedSeconds`/`compareFrame*`/`compareHover*` fields; add a single `compare: null,`.

- [ ] **Step 6: Update `web/app.js` — event wiring**

In the wiring section (~line 445), replace the compare listeners with:

```js
  elements.compareButton.addEventListener("click", openCompare);
  elements.compareClose.addEventListener("click", closeCompare);
  elements.compareOverlay.addEventListener("click", (event) => {
    if (event.target === elements.compareOverlay) closeCompare();
  });
  elements.compareSlider.addEventListener("input", updateCompareDivider);
  elements.comparePlay.addEventListener("click", toggleComparePlayback);
  elements.compareMute.addEventListener("click", toggleCompareMute);
  elements.compareTimeline.addEventListener("input", handleCompareTimelineInput);
  elements.compareTimeline.addEventListener("pointerdown", beginCompareScrub);
  elements.compareTimeline.addEventListener("pointermove", previewCompareTimeline);
  elements.compareTimeline.addEventListener("pointerleave", hideCompareHoverPreview);
  elements.compareCompressedVideo.addEventListener("ended", handleCompareEnded);
  document.addEventListener("pointerup", endCompareScrub);
  document.addEventListener("pointercancel", endCompareScrub);
  document.addEventListener("keydown", handleCompareKeydown);
  wireCompareVideo("input");
  wireCompareVideo("output");
```

- [ ] **Step 7: Update `web/app.js` — replace the compare implementation**

Delete everything from `function openCompare()` through the end of `function handleCompareKeydown(event)` (the whole stills-era block, ~lines 875-1137: `openCompare`, `closeCompare`, `cancelCompareFrameLoad`, `releaseCompareFrameURLs`, `releaseCompareHoverURLs`, `hideCompareHoverPreview`, `clampCompareSeconds`, `updateCompareTimelinePosition`, `scheduleCompareFrame`, `handleCompareTimelineInput`, `previewCompareTimeline`, `fetchCompareFrame`, `createDecodedCompareFrameURLs`, `createDecodedCompareHoverURL`, `scheduleCompareHoverFrame`, `loadCompareHoverFrames`, `loadCompareFrames`, `handleCompareKeydown` — keep `updateCompareDivider`). Insert this complete replacement:

```js
const COMPARE_PROFILES = {
  h264mp4: 'video/mp4; codecs="avc1.64002A, mp4a.40.2"',
  vp9webm: 'video/webm; codecs="vp09.00.40.08, opus"',
};

function videoFor(side) {
  return side === "input" ? elements.compareOriginalVideo : elements.compareCompressedVideo;
}

function compareMediaURL(side, variant) {
  return `/api/compare/media/${side}?variant=${variant}&token=${encodeURIComponent(token)}`;
}

function canPlayCompareType(mime) {
  if (!mime) return false;
  return elements.compareCompressedVideo.canPlayType(mime) !== "";
}

// wireCompareVideo attaches the per-side media listeners once at startup; the
// handlers read state.compare so stale events after close are ignored.
function wireCompareVideo(side) {
  const video = videoFor(side);
  video.addEventListener("loadedmetadata", () => handleCompareVideoReady(side));
  video.addEventListener("error", () => handleCompareVideoError(side));
}

async function openCompare() {
  if (!elements.compareOverlay.hidden || elements.compareButton.hidden) return;
  const compare = {
    duration: 0,
    sides: null,
    sources: { input: null, output: null },
    fallbackTried: { input: false, output: false },
    readySides: { input: false, output: false },
    progress: { input: 0, output: 0 },
    convertTimers: { input: null, output: null },
    storyboard: null,
    storyboardImage: null,
    storyboardTimer: null,
    storyboardTries: 0,
    rafId: null,
    syncTimer: null,
    seekRaf: null,
    pendingSeek: 0,
    scrubbing: false,
    failure: "",
  };
  state.compare = compare;
  elements.compareSlider.value = "50";
  updateCompareDivider();
  elements.compareTimeline.value = "0";
  elements.compareTimeline.disabled = true;
  elements.comparePlay.disabled = true;
  elements.comparePlay.classList.remove("playing");
  elements.compareMute.disabled = true;
  elements.compareMute.classList.remove("muted");
  elements.compareLoading.textContent = "Loading video previews…";
  elements.compareLoading.hidden = false;
  elements.compareHint.textContent = "Play or scrub the timeline · hover for an instant preview.";
  document.body.classList.add("compare-open");
  elements.compareOverlay.hidden = false;
  elements.compareClose.focus();
  try {
    const opened = await api("/api/compare/open", { method: "POST", body: "{}" });
    if (state.compare !== compare) return;
    compare.duration = Number(opened.duration) || 0;
    compare.sides = opened.sides || {};
    elements.compareTimeline.max = String(Math.max(1, Math.round(compare.duration * 1000)));
    elements.compareTime.textContent = `${formatDuration(0)} / ${formatDuration(compare.duration)}`;
    initCompareStoryboard(opened.storyboard);
    for (const side of ["input", "output"]) {
      const preview = opened.previews?.[side];
      if (preview?.state === "ready") {
        attachCompareSource(side, "preview");
      } else if (preview?.state === "converting") {
        compare.sources[side] = "converting";
        pollCompareConvert(side);
      } else {
        chooseCompareSource(side);
      }
    }
    updateCompareLoading();
  } catch (error) {
    if (state.compare !== compare) return;
    failCompare(error.message);
  }
}

function closeCompare() {
  if (elements.compareOverlay.hidden) return;
  const compare = state.compare;
  state.compare = null;
  if (compare) {
    if (compare.rafId) cancelAnimationFrame(compare.rafId);
    if (compare.seekRaf) cancelAnimationFrame(compare.seekRaf);
    if (compare.syncTimer) clearInterval(compare.syncTimer);
    if (compare.storyboardTimer) clearTimeout(compare.storyboardTimer);
    for (const side of ["input", "output"]) {
      if (compare.convertTimers[side]) clearTimeout(compare.convertTimers[side]);
    }
  }
  hideCompareHoverPreview();
  // Releasing the src frees both decoders; server-side assets stay cached so
  // reopening is instant.
  for (const video of [elements.compareOriginalVideo, elements.compareCompressedVideo]) {
    video.pause();
    video.removeAttribute("src");
    video.load();
  }
  elements.comparePlay.classList.remove("playing");
  document.body.classList.remove("compare-open");
  elements.compareOverlay.hidden = true;
  elements.compareButton.focus();
}

// chooseCompareSource plays the real file whenever the browser can decode it
// (Matroska optimistically — canPlayType under-reports it) and converts
// otherwise. A direct attempt that errors falls back through
// handleCompareVideoError exactly once.
function chooseCompareSource(side) {
  const info = state.compare?.sides?.[side];
  if (!info) return;
  if (info.optimistic || canPlayCompareType(info.fullMime)) {
    attachCompareSource(side, "source");
  } else {
    startCompareConvert(side);
  }
}

function attachCompareSource(side, variant) {
  const compare = state.compare;
  if (!compare) return;
  compare.sources[side] = variant;
  compare.readySides[side] = false;
  const video = videoFor(side);
  video.src = compareMediaURL(side, variant);
  video.load();
}

function handleCompareVideoReady(side) {
  const compare = state.compare;
  if (!compare || compare.readySides[side]) return;
  compare.readySides[side] = true;
  if (compare.readySides.input && compare.readySides.output) finishCompareOpen();
}

function finishCompareOpen() {
  const compare = state.compare;
  if (!compare) return;
  elements.compareTimeline.disabled = !compare.duration;
  elements.comparePlay.disabled = false;
  const hasAudio = Boolean(compare.sides?.output?.hasAudio);
  elements.compareMute.disabled = !hasAudio;
  elements.compareMute.title = hasAudio ? "Mute audio" : "The compressed output has no audio track";
  elements.compareMute.classList.toggle("muted", !hasAudio);
  elements.compareCompressedVideo.muted = !hasAudio;
  elements.compareLoading.hidden = true;
  seekCompare(Math.min(1, compare.duration));
  startCompareSync();
}

function handleCompareVideoError(side) {
  const compare = state.compare;
  if (!compare || elements.compareOverlay.hidden) return;
  if (compare.sources[side] === "source" && !compare.fallbackTried[side]) {
    compare.fallbackTried[side] = true;
    startCompareConvert(side);
    return;
  }
  if (compare.sources[side] === null) return;
  failCompare(side === "input" ? "The original video could not be played." : "The compressed video could not be played.");
}

function failCompare(message) {
  const compare = state.compare;
  if (!compare) return;
  compare.failure = message;
  elements.comparePlay.disabled = true;
  elements.compareTimeline.disabled = true;
  elements.compareLoading.textContent = message;
  elements.compareLoading.hidden = false;
}

async function startCompareConvert(side) {
  const compare = state.compare;
  const info = compare?.sides?.[side];
  if (!compare || !info) return;
  compare.sources[side] = "converting";
  updateCompareLoading();
  try {
    const started = await api("/api/compare/convert", {
      method: "POST",
      body: JSON.stringify({
        side,
        verdicts: {
          full: canPlayCompareType(info.fullMime),
          video: canPlayCompareType(info.videoMime),
          audio: canPlayCompareType(info.audioMime),
        },
        profiles: {
          h264mp4: canPlayCompareType(COMPARE_PROFILES.h264mp4),
          vp9webm: canPlayCompareType(COMPARE_PROFILES.vp9webm),
        },
      }),
    });
    if (state.compare !== compare) return;
    applyCompareConvertState(side, started);
  } catch (error) {
    if (state.compare !== compare) return;
    failCompare(error.message);
  }
}

function applyCompareConvertState(side, convert) {
  const compare = state.compare;
  if (!compare) return;
  if (convert.state === "ready") {
    attachCompareSource(side, "preview");
    updateCompareLoading();
    return;
  }
  if (convert.state === "failed") {
    failCompare(convert.error || "The playable preview could not be prepared.");
    return;
  }
  compare.sources[side] = "converting";
  compare.progress[side] = Number(convert.progress) || 0;
  updateCompareLoading();
  if (!compare.convertTimers[side]) {
    compare.convertTimers[side] = setTimeout(() => {
      compare.convertTimers[side] = null;
      pollCompareConvert(side);
    }, 500);
  }
}

async function pollCompareConvert(side) {
  const compare = state.compare;
  if (!compare) return;
  try {
    const convert = await api(`/api/compare/convert/${side}`);
    if (state.compare !== compare) return;
    applyCompareConvertState(side, convert);
  } catch (error) {
    if (state.compare !== compare) return;
    failCompare(error.message);
  }
}

function updateCompareLoading() {
  const compare = state.compare;
  if (!compare || compare.failure) return;
  const lines = [];
  if (compare.sources.input === "converting") {
    lines.push(`Preparing a playable original preview… ${Math.round(100 * compare.progress.input)}%`);
  }
  if (compare.sources.output === "converting") {
    lines.push(`Preparing a playable compressed preview… ${Math.round(100 * compare.progress.output)}%`);
  }
  if (lines.length) {
    elements.compareLoading.textContent = lines.join(" · ");
    elements.compareLoading.hidden = false;
  } else if (!(compare.readySides.input && compare.readySides.output)) {
    elements.compareLoading.textContent = "Loading video previews…";
    elements.compareLoading.hidden = false;
  }
}

// The compressed side is the master clock and the audio source; the original
// follows and stays muted.
function toggleComparePlayback() {
  const compare = state.compare;
  if (!compare || elements.comparePlay.disabled) return;
  const master = elements.compareCompressedVideo;
  const original = elements.compareOriginalVideo;
  if (master.paused) {
    if (master.ended) seekCompare(0);
    original.play().catch(() => {});
    master.play().catch(() => {});
    elements.comparePlay.classList.add("playing");
    startCompareClock();
  } else {
    master.pause();
    original.pause();
    elements.comparePlay.classList.remove("playing");
  }
}

function handleCompareEnded() {
  elements.compareOriginalVideo.pause();
  elements.comparePlay.classList.remove("playing");
  updateCompareClock();
}

function toggleCompareMute() {
  if (elements.compareMute.disabled) return;
  const muted = !elements.compareCompressedVideo.muted;
  elements.compareCompressedVideo.muted = muted;
  elements.compareMute.classList.toggle("muted", muted);
}

function startCompareClock() {
  const compare = state.compare;
  if (!compare || compare.rafId) return;
  const step = () => {
    const current = state.compare;
    if (!current) return;
    current.rafId = null;
    updateCompareClock();
    if (!elements.compareCompressedVideo.paused) {
      current.rafId = requestAnimationFrame(step);
    }
  };
  compare.rafId = requestAnimationFrame(step);
}

function updateCompareClock() {
  const compare = state.compare;
  if (!compare) return;
  const seconds = Math.min(compare.duration, elements.compareCompressedVideo.currentTime);
  if (!compare.scrubbing) {
    elements.compareTimeline.value = String(Math.round(seconds * 1000));
  }
  elements.compareTime.textContent = `${formatDuration(seconds)} / ${formatDuration(compare.duration)}`;
}

// startCompareSync keeps the two local streams within 60ms of each other; a
// nudge on the muted side is invisible, and local files never drift far.
function startCompareSync() {
  const compare = state.compare;
  if (!compare || compare.syncTimer) return;
  compare.syncTimer = setInterval(() => {
    const master = elements.compareCompressedVideo;
    const original = elements.compareOriginalVideo;
    if (master.paused || master.seeking || original.seeking) return;
    if (Math.abs(original.currentTime - master.currentTime) > 0.06) {
      original.currentTime = master.currentTime;
    }
  }, 500);
}

// seekCompare coalesces seeks through one rAF so dragging the timeline issues
// at most one seek per frame; native seeking on local files needs no debounce.
function seekCompare(seconds) {
  const compare = state.compare;
  if (!compare) return;
  compare.pendingSeek = Math.max(0, Math.min(compare.duration, Number(seconds) || 0));
  if (compare.seekRaf) return;
  compare.seekRaf = requestAnimationFrame(() => {
    const current = state.compare;
    if (!current) return;
    current.seekRaf = null;
    elements.compareCompressedVideo.currentTime = current.pendingSeek;
    elements.compareOriginalVideo.currentTime = current.pendingSeek;
    updateCompareClock();
  });
}

function handleCompareTimelineInput() {
  hideCompareHoverPreview();
  seekCompare(Number(elements.compareTimeline.value || 0) / 1000);
}

function beginCompareScrub() {
  if (state.compare) state.compare.scrubbing = true;
  hideCompareHoverPreview();
}

function endCompareScrub() {
  if (state.compare) state.compare.scrubbing = false;
}

function initCompareStoryboard(manifest) {
  const compare = state.compare;
  if (!compare) return;
  compare.storyboard = manifest || null;
  if (!manifest) return;
  if (manifest.state === "ready") {
    loadCompareStoryboardImage();
  } else if (manifest.state !== "failed") {
    scheduleCompareStoryboardPoll();
  }
}

function scheduleCompareStoryboardPoll() {
  const compare = state.compare;
  if (!compare || compare.storyboardTimer || compare.storyboardTries >= 60) return;
  compare.storyboardTimer = setTimeout(async () => {
    const current = state.compare;
    if (current !== compare) return;
    compare.storyboardTimer = null;
    compare.storyboardTries += 1;
    try {
      const manifest = await api("/api/compare/storyboard/manifest");
      if (state.compare !== compare) return;
      compare.storyboard = manifest;
      if (manifest.state === "ready") {
        loadCompareStoryboardImage();
      } else if (manifest.state !== "failed") {
        scheduleCompareStoryboardPoll();
      }
    } catch {
      if (state.compare === compare) scheduleCompareStoryboardPoll();
    }
  }, 1000);
}

function loadCompareStoryboardImage() {
  const compare = state.compare;
  if (!compare || compare.storyboardImage) return;
  const image = new Image();
  image.src = `/api/compare/storyboard?token=${encodeURIComponent(token)}`;
  image
    .decode()
    .then(() => {
      if (state.compare !== compare || !compare.storyboard) return;
      compare.storyboardImage = image;
      const story = compare.storyboard;
      elements.compareHoverStage.style.aspectRatio = `${story.tileWidth} / ${story.tileHeight}`;
      elements.compareHoverThumb.style.backgroundImage = `url("${image.src}")`;
    })
    .catch(() => {});
}

// previewCompareTimeline is a pure local lookup: position the tooltip, then
// point the sprite background at the hovered tile. No network per hover.
function previewCompareTimeline(event) {
  const compare = state.compare;
  if (!compare || elements.compareOverlay.hidden || !compare.duration || elements.compareTimeline.disabled) return;
  if (event.buttons !== 0) {
    hideCompareHoverPreview();
    return;
  }
  const bounds = elements.compareTimeline.getBoundingClientRect();
  if (!bounds.width) return;
  const position = Math.max(0, Math.min(1, (event.clientX - bounds.left) / bounds.width));
  const seconds = compare.duration * position;
  const wrapperBounds = elements.compareTimelineWrap.getBoundingClientRect();
  const previewHalfWidth = Math.min(96, wrapperBounds.width / 2);
  const pointerX = event.clientX - wrapperBounds.left;
  const previewCenter = Math.max(previewHalfWidth, Math.min(wrapperBounds.width - previewHalfWidth, pointerX));
  elements.compareHoverPreview.style.left = `${previewCenter}px`;
  elements.compareHoverTime.textContent = formatDuration(seconds);
  elements.compareHoverPreview.hidden = false;
  const story = compare.storyboard;
  if (compare.storyboardImage && story?.state === "ready" && story.interval > 0) {
    const index = Math.min(story.count - 1, Math.floor(seconds / story.interval));
    const column = index % story.columns;
    const row = Math.floor(index / story.columns);
    elements.compareHoverThumb.style.backgroundPosition = `${-column * story.tileWidth}px ${-row * story.tileHeight}px`;
    elements.compareHoverPreview.classList.remove("loading");
  } else {
    elements.compareHoverPreview.classList.add("loading");
  }
}

function hideCompareHoverPreview() {
  elements.compareHoverPreview.classList.remove("loading");
  elements.compareHoverPreview.hidden = true;
}

function handleCompareKeydown(event) {
  if (elements.compareOverlay.hidden) return;
  const inRange = event.target instanceof HTMLInputElement && event.target.type === "range";
  if (event.key === "Escape") {
    event.preventDefault();
    closeCompare();
  } else if (event.key === " " && !inRange) {
    event.preventDefault();
    toggleComparePlayback();
  } else if ((event.key === "ArrowLeft" || event.key === "ArrowRight") && !inRange) {
    event.preventDefault();
    const delta = event.key === "ArrowLeft" ? -5 : 5;
    seekCompare(elements.compareCompressedVideo.currentTime + delta);
  }
}
```

Keep `updateCompareDivider` exactly as it is. Check for other references to the deleted names (`grep -n "compareOriginalFrame\|compareCompressedFrame\|compareHoverFrame\|compareLoadedSeconds\|compareDuration" web/app.js`) — `openCompare`'s old references are gone with the rewrite; fix any strays (e.g. `state.compareDuration` uses inside `handleJobCompleted`/polling code, if present, become `state.compare?.duration` or are deleted).

- [ ] **Step 8: Run the contract test to verify it passes**

Run: `go test -run TestCompletedCompressionOffersLargeSynchronizedComparison ./...`
Expected: PASS. Then the full suite: `go test ./...` — PASS.

- [ ] **Step 9: Syntax-check the JavaScript**

Run: `node --check web/app.js` (Node is available on Fedora dev machines; if not, `deno check` or skip with a browser smoke test in Task 10).
Expected: no output (clean parse).

- [ ] **Step 10: Commit**

```bash
git add server_test.go web/index.html web/styles.css web/app.js
git commit -m "Replace stills comparison with synced video playback and sprite hovers"
```

---

### Task 10: Docs, version, end-to-end verification

**Files:**
- Modify: `README.md` (Features bullet)
- Modify: `main.go:28` (`version`)

**Interfaces:** none — this is polish + verification.

- [ ] **Step 1: Rewrite the README feature bullet**

Replace the "Visual comparison" bullet in `README.md` with:

```markdown
- **Visual comparison**: after a successful compression, open a large before/after viewer and press play — both sides run in sync with the compressed track's audio, the divider stays draggable mid-playback, seeking is native and instant, and hovering the timeline shows storyboard thumbnails with zero decoding lag. Codecs the browser cannot decode (H.266, most H.265) get a one-time playable preview generated in the background and cleaned up automatically.
```

Also extend the "Drag & drop that behaves" bullet to mention the deeper search (recent-files first, then a bounded scan that includes external drives), reflecting the locate work that shipped alongside this release.

- [ ] **Step 2: Bump the version**

In `main.go` line 28: `const version = "1.9.0"` — this release carries both the comparison playback overhaul and the drag-and-drop locate improvements.

- [ ] **Step 3: Full verification**

```bash
gofmt -l .            # expect: no output
go vet ./...          # expect: clean
go build ./...        # expect: clean
go test ./...         # expect: ok exactsize
node --check web/app.js
```

- [ ] **Step 4: Manual end-to-end check (real browser)**

Launch the app (`go run . ` or the built binary; `EXACTSIZE_HEADLESS=1 go run .` prints the URL to open manually). With any local H.264 MP4:
1. Compress it to a small target; when it completes, open Compare.
2. Both sides appear ≤1s (direct playback); press Play — motion + audio, divider draggable during playback.
3. Drag the timeline: both sides seek instantly, no spinner.
4. Hover the timeline: thumbnail appears instantly and tracks the pointer with no network requests (DevTools Network stays quiet during hover).
5. Toggle mute; Space toggles play; arrows jump 5s; Esc closes; reopen — instant.
6. If an H.265/H.266 encoder is available, compress to H.265: opening Compare shows "Preparing a playable compressed preview… N%" then plays with audio.
7. Start a new encode with the viewer open in another window state — the viewer surfaces the 409 as a failure message rather than crashing.

If no display is available, verify at the HTTP level with curl: `/api/compare/open` (200 JSON), `/api/compare/media/output?variant=source&token=…` with a `Range` header (206), `/api/compare/storyboard/manifest` (ready after a completed encode).

- [ ] **Step 5: Commit**

```bash
git add README.md main.go
git commit -m "Release 1.9.0 with comparison playback and drop-location fixes"
```
