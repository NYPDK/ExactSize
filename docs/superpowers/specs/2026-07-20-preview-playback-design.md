# Comparison playback overhaul: audio, lag-free seeking, instant hover thumbnails

- **Date:** 2026-07-20
- **Status:** Approved (approach and audio source confirmed by owner)
- **Version target:** 1.9.0

## Problem

The "Original vs compressed" viewer is stills-only. Every scrub position spawns a
fresh FFmpeg process server-side that seeks and extracts one PNG per side
(`/api/compare/frame/{side}`), and every timeline hover does the same for a
thumbnail. Each unique position costs process spawn + demux seek + decode + PNG
encode + HTTP transfer — typically 150 ms to over a second. There is no playback
and no audio at all.

Goals, in the owner's words: audio, no lag, and faster preview loading when
hovering the timeline/seek bar.

## Goals

1. Real playback with audio in the compare viewer (play/pause, honest sound).
2. Lag-free seeking and frame inspection (native `<video>` seeking, no
   per-frame server round trips).
3. Instant hover thumbnails on the timeline (no network or FFmpeg per hover).
4. Works for every codec the app can produce — including H.266 and H.265,
   which browsers cannot (reliably) decode — and for arbitrary input files.

## Non-goals

- No frame-step buttons, playback-speed control, loop mode, or A/B audio
  toggle (audio is always the compressed output's — decided by owner).
- No change to encoding, remux, or job logic.
- No support for comparing older jobs; comparison stays scoped to the current
  completed, non-remux, non-mux job, as today.

## Decisions already made by the owner

- **Approach:** direct playback of the real files whenever the browser can
  decode them; a one-time background conversion to a temp preview file only
  for sides the browser cannot play. (Rejected: always-pre-encode; live
  Plex-style streaming.)
- **Audio:** always the compressed output's audio track. If audio was dropped
  in the encode, the preview plays silence and the mute button is disabled
  with a "no audio track" hint.
- The README's "no preview encodes, no temporary files" claim is consciously
  retired; temp previews are bounded, job-scoped, and auto-cleaned.

## Current-state facts the design relies on (verified)

- The UI runs in a Chromium-family `--app` window (Brave/Chrome/Chromium/Edge
  preferred, Firefox fallback). CSP already allows `media-src 'self'` and
  `img-src 'self'`.
- The frontend token comes from the page URL (`?token=`); API calls send it as
  the `X-ExactSize-Token` header. Media elements cannot send headers, so media
  GETs must also accept `?token=`.
- Bundled FFmpeg is the BtbN GPL static build; the build script asserts
  `libx264`, `libx265`, `libvvenc`, `libaom-av1`, `libvpx-vp9`, `aac`,
  `libopus`, `libvorbis`, `libmp3lame` encoders exist. The conversion fallback
  can rely on libx264 + aac and libvpx-vp9 + libopus.
- `server.go` already ships temp-dir infrastructure for compare previews:
  `comparePreviewPrefix = "exactsize-compare-"` and
  `cleanupStaleComparePreviews()` (PID-scoped dirs, stale sweep at launch).
  Nothing currently creates those dirs; this design revives them.
- Two tests currently *enforce* stills-only ("still-frame comparison retains
  playback behavior") and must be rewritten to enforce playback instead.
- Only one job exists at a time (`App.job`); comparison requires
  `state == "completed" && !Remux && !MuxAudio`.

## Architecture overview

```
openCompare()
  └─ POST /api/compare/open ──────────────► probe both files (cached),
       ◄─ per-side {mime strings, meta},     ensure storyboard job started
          storyboard manifest state,
          converted-preview states
  per side: canPlayType(fullMime) ≠ "" ?
     yes → <video src="/api/compare/media/{side}?variant=source&token=…">
           (Range-served real file; on <video> 'error' → fall through ↓)
     no  → POST /api/compare/convert {side, verdicts}
           poll GET /api/compare/convert/{side} → progress overlay
           ready → <video src=".../media/{side}?variant=preview&token=…">
playback: compressed <video> = master clock + audio; original muted,
          drift-nudged; timeline = real seeks on both
hover:    storyboard sprite JPEG + manifest → CSS background-position math,
          zero network per hover
```

The per-frame PNG pipeline (`handleCompareFrame`, `extractCompareFrame`, the
route, and all frontend fetch/decode/objectURL/abort machinery for frames and
hover thumbs) is deleted.

## Server design

New file `compare.go` (plus `compare_test.go`) holding the compare-assets
manager and handlers; `server.go` keeps only the route registrations.

### Compare assets manager

```go
type compareAssets struct {
    job     *Job              // owning job; requests re-verify identity
    dir     string            // exactsize-compare-<pid>-<rand> under os.TempDir(), created lazily
    ctx     context.Context   // canceled on teardown
    cancel  context.CancelFunc
    mu      sync.Mutex
    probes  map[string]VideoInfo      // "input", "output"
    story   storyboardState           // pending|generating|ready|failed + manifest
    convert map[string]*convertState  // per side: none|converting|ready|failed, progress, error, path
}
```

Held as `App.compare`, guarded by `App.mu`. Lifecycle:

- **Created** when a non-remux, non-mux job reaches `completed`. In
  `handleStartJob`, the goroutine becomes
  `go func() { job.run(...); a.prepareCompareAssets(job) }()`;
  `prepareCompareAssets` checks the final snapshot and, when eligible, builds
  the assets struct and starts storyboard generation in the background.
- **Torn down** (cancel ctx → kills FFmpeg children; `os.RemoveAll(dir)`) when
  a new job is accepted in `handleStartJob`, and in `cancelCurrentJob` (app
  shutdown). `cleanupStaleComparePreviews()` remains the crash backstop.
- Converted previews and the storyboard persist across viewer close/reopen
  within the same job (reopen is instant).

### Endpoints

All under existing header auth; the two media-serving GETs additionally accept
`?token=` compared with `crypto/subtle.ConstantTimeCompare`.

**`POST /api/compare/open`** — 404/409 as today via `currentComparisonJob`.
Probes input and output with `probeVideo` (cached in `probes`). Response:

```json
{
  "duration": 42.1,
  "sides": {
    "input":  { "fullMime": "video/mp4; codecs=\"avc1.64002A, mp4a.40.2\"",
                "videoMime": "video/mp4; codecs=\"avc1.64002A\"",
                "audioMime": "audio/mp4; codecs=\"mp4a.40.2\"",
                "hasAudio": true, "width": 1920, "height": 1080,
                "duration": 42.15 },
    "output": { "…same shape…" }
  },
  "storyboard": { "state": "generating",
                  "interval": 0.5, "count": 84, "columns": 10,
                  "tileWidth": 192, "tileHeight": 108 },
  "previews": { "input":  { "state": "none", "progress": 0 },
                "output": { "state": "ready", "progress": 1 } }
}
```

`duration` = `max(0.1, min(input.duration, output.duration) - 0.05)` — the
timeline ceiling, clamped so seeking never lands past either stream's end.

MIME construction is a pure Go function (`compareMime(info VideoInfo) sideMimes`).
The three mimes serve two distinct purposes:

- **`fullMime`** — the *source* container + codecs, used only for the
  direct-play decision.
- **`videoMime` / `audioMime`** — each codec expressed in the container
  `planConversion` would remux it into (its *target* container), used for the
  convert verdicts. This sidesteps `canPlayType`'s unreliability for
  containers like Matroska: what the planner needs to know is whether the
  browser can decode the codec in the container a preview would actually use.

| Probe fact | Mapping |
| --- | --- |
| source container ext `.mp4`, `.m4v`, `.mov` | fullMime base `video/mp4` (MOV is ISO-BMFF family; Chromium demuxes it as MP4, and `video/quicktime` would always report unplayable) |
| `.webm` | fullMime base `video/webm` |
| `.mkv` | fullMime base `video/x-matroska`, **optimistic**: the client attempts direct play regardless of `canPlayType` (Chromium plays Matroska but under-reports it; Firefox fires the `error` event and falls into the convert flow, which remuxes cheaply) |
| anything else (`.avi`, `.ts`, `.wmv`, …) | fullMime `""` → straight to convert |
| video `h264` | `avc1.64002A`, target container `video/mp4` |
| `hevc` | `hvc1.1.6.L123.B0`, target `video/mp4` |
| `av1` | `av01.0.08M.08` (`av01.0.08M.10` when `pix_fmt` contains `10le`), target `video/mp4` |
| `vp9` | `vp09.00.40.08`, target `video/webm` |
| `vvc`/other/unknown | `""` → videoMime `""` → video verdict false |
| audio `aac` | `mp4a.40.2`, target `audio/mp4`; `mp3` → `mp3`, target `audio/mp4` |
| audio `opus` | `opus`, target `audio/webm`; `vorbis` → `vorbis`, target `audio/webm` |
| other audio | audioMime `""` → audio verdict false |

An empty `fullMime` means "don't even try direct". `canPlayType` returning
`"maybe"` counts as playable; the `<video>` `error` event is the safety net
for false positives (notably H.265 on Chromium builds without HEVC support).

**`GET /api/compare/media/{side}?variant=source|preview&token=…`** —
`side ∈ {input, output}`; `variant=source` serves `job.request.Input`/`.Output`,
`variant=preview` serves the converted file (404 until `ready`). Served with
`http.ServeContent` (Range support = native seeking) and an explicit
`Content-Type` from the extension table above. Re-verifies the owning job is
still current (409 otherwise).

**`POST /api/compare/convert`** — body:

```json
{ "side": "output",
  "verdicts": { "full": false, "video": false, "audio": true },
  "profiles": { "h264mp4": true, "vp9webm": true } }
```

`verdicts` are the client's `canPlayType` results for that side's three mimes;
`profiles` are `canPlayType` results for the two candidate preview profiles
(`video/mp4; codecs="avc1.64002A, mp4a.40.2"`,
`video/webm; codecs="vp09.00.40.08, opus"`). Idempotent: returns the existing
state if a conversion is already running or done. Starts FFmpeg on the assets
ctx, parsing `-progress pipe:1` (`out_time_us` ÷ side duration → progress;
stdout is free because output goes to the temp file). If the viewer is
reopened while a conversion runs, `/open` reports `converting` and the client
simply resumes polling.

The plan is a pure, table-tested function:

```go
func planConversion(info VideoInfo, v verdicts, p profiles) (convertPlan, error)
```

| Case | Plan |
| --- | --- |
| `v.video && v.audio` (or no audio track) | Remux, `-c copy`: container `mp4` (`+faststart`) if video ∈ {h264, h265, av1} and audio ∈ {aac, mp3, opus, none}; else `webm` if video ∈ {vp9, av1} and audio ∈ {opus, vorbis, none}; else fall through to audio-transcode row with video copy |
| `v.video && !v.audio` | `-c:v copy`, audio → `aac 160k` into `mp4` (video ∈ {h264, h265, av1}) or `libopus 128k` into `webm` (video ∈ {vp9, av1}) |
| `!v.video`, `p.h264mp4` | `libx264 -preset veryfast -crf 18 -pix_fmt yuv420p`, scale cap `scale=w='min(1920,iw)':h='min(1920,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2` (same idiom as the old frame extractor), audio copy if `v.audio` and codec fits mp4, else `aac 160k`, `-movflags +faststart` |
| `!v.video`, `!p.h264mp4`, `p.vp9webm` | `libvpx-vp9 -deadline realtime -cpu-used 5 -row-mt 1 -crf 24 -b:v 0 -pix_fmt yuv420p`, same scale cap, audio copy if playable and webm-legal else `libopus 128k` |
| neither profile playable | error: "this browser cannot play any preview format" |

Every plan maps only the first video and first audio stream (`-map 0:v:0`,
`-map 0:a:0?`), drops subtitles/data, and uses `-nostdin -hide_banner
-loglevel error`.

**`GET /api/compare/convert/{side}`** —
`{ "state": "none|converting|ready|failed", "progress": 0.73, "error": "" }`.
On failure, `error` carries the trimmed FFmpeg stderr tail (last ~4 KB).

**`GET /api/compare/storyboard?token=…`** — the sprite JPEG (404 until ready).
**`GET /api/compare/storyboard/manifest`** — the storyboard state object (same
shape as in `/open`); the client polls this at 1 s while `generating`, giving
up silently after 60 attempts (tooltip stays time-only).

### Storyboard generator

Runs once per completed job, started from `prepareCompareAssets` (so it is
usually finished before the user opens the viewer — generation starts while
they are still reading the completion stats). Input: the **output** file (hover
shows compressed frames, matching today's behavior — and it works even for
H.266 because FFmpeg decodes it server-side regardless of the browser).

- `count = clamp(round(duration), 16, 180)`, `interval = duration / count`,
  `columns = 10`, `rows = ceil(count / columns)`.
- `tileWidth = 192` (matches the existing 192 px hover box);
  `tileHeight = round(192 * height / width / 2) * 2`.
- Command: `-i OUT -map 0:v:0 -an -sn -dn
  -vf "fps=<count>/<duration>,scale=192:-2,tile=<cols>x<rows>" -frames:v 1
  -q:v 4 -f image2 <dir>/storyboard.jpg`.
- FPS rounding can produce slightly fewer frames than `count`; trailing tiles
  are black. The client clamps the index to `count - 1`, and equality of
  `floor(t/interval)` with actual tile positions keeps mismatch ≤ 1 tile.
- Failure is non-fatal: state `failed`, hover falls back to time-only tooltip.

## Frontend design

### Markup (`index.html`)

- Stage: the two `<img>` frames become
  `<video class="compare-frame compare-original" id="compareOriginalVideo" muted playsinline preload="auto">`
  and
  `<video class="compare-frame compare-compressed" id="compareCompressedVideo" playsinline preload="auto">`.
  Labels, divider, split slider, loading element unchanged.
- Toolbar gains, left of the timeline: play/pause button (`id="comparePlay"`,
  inline SVG icon swap) and mute button (`id="compareMute"`). Timeline, time
  readout, hint remain.
- Hover preview: `<img id="compareHoverFrame">` becomes
  `<div class="compare-hover-thumb" id="compareHoverThumb"></div>` — sized from
  the manifest (192 × tileHeight via JS style properties), sprite as
  `background-image`, tile chosen via `background-position`.

### Open flow (`openCompare`)

1. Show modal with "Loading video previews…".
2. `POST /api/compare/open`; store response. Timeline max = `duration * 1000`
   (ms units, as today).
3. Per side, pick the source:
   - converted preview already `ready` → preview URL;
   - else `canPlayType(fullMime) !== ""` → source URL (optimistic direct);
   - else → convert flow (below).
4. Assign `src` (with `?token=`), wait for both sides' `loadedmetadata`; then
   seek both to `min(1, duration)`, paused, hide loading. Hint text:
   "Play or scrub the timeline · hover for an instant preview."
5. Storyboard: if `ready`, prefetch the sprite into an `Image` and enable
   hover thumbs; if `generating`, poll the manifest (1 s, ≤ 60 tries).

### Convert flow (per side)

1. `POST /api/compare/convert` with that side's verdicts + profile support.
2. Poll `GET /api/compare/convert/{side}` at 500 ms; the stage loading element
   shows per-side lines: "Preparing playable preview (compressed)… 42%".
3. `ready` → assign preview URL and continue the open flow.
4. `failed` → stage shows the error text; the other side may still work, but
   playback controls stay disabled without both sides.

A `<video>` `error` event on an optimistic direct source automatically enters
this flow (guarding against one retry only — a side that fails direct AND
converted shows the failure message).

### Playback, sync, seeking

- Master clock and audio: compressed video. Original stays `muted`.
- `togglePlay()` (button + Space): if master ended, reset both to 0 first;
  `play()` both (original first, master second), pause pauses both. The play
  gesture satisfies Chromium's autoplay-with-sound policy.
- rAF loop while open and playing: timeline value + time text from
  `master.currentTime`, suppressed while the user is dragging the timeline
  (pointerdown → pointerup/cancel).
- Drift loop, 500 ms while playing: if
  `|original.currentTime - master.currentTime| > 0.06` →
  `original.currentTime = master.currentTime`. One immediate sync after each
  seek. (Local files keep drift far below the threshold in practice.)
- Timeline `input`: rAF-throttled `seekBoth(t)` — sets both `currentTime`
  (no debounce timers, no aborts; native seeks coalesce). `change` seeks
  immediately. Playback state is preserved through scrubbing.
- Keyboard: Space = play/pause (`preventDefault` unless focus is in a range
  input), ArrowLeft/Right = ±5 s when focus is not in a range input (ranges
  keep their native arrow behavior), Escape = close (unchanged).
- Mute button: toggles `master.muted`; when `output.hasAudio` is false it is
  disabled with title "The compressed output has no audio track" and the
  viewer stays silent (honest preview).
- `closeCompare()`: pause both, cancel rAF/intervals/polls, `removeAttribute("src")`
  + `load()` on both videos (releases decoders), keep server-side assets.

### Hover thumbnails

`previewCompareTimeline` keeps its positioning math, but the data path is:
`index = min(count - 1, floor(t / interval))`;
`background-position = -(index % columns) * tileWidth px,
-floor(index / columns) * tileHeight px`. No network, no debounce, no abort
machinery — the sprite was prefetched once. Until the manifest is `ready`, the
thumb area shows the existing loading shimmer with the timestamp (no requests
from hover events, ever).

### Deleted frontend machinery

`fetchCompareFrame`, `createDecodedCompareFrameURLs`,
`createDecodedCompareHoverURL`, `loadCompareFrames`, `loadCompareHoverFrames`,
`scheduleCompareFrame`, `scheduleCompareHoverFrame`, and the
`compareFrame*`/`compareHover*` abort/objectURL/timer state fields.

## Error handling summary

| Failure | Behavior |
| --- | --- |
| Direct play errors (codec false positive) | Auto-switch that side to convert; one retry layer only |
| Conversion FFmpeg fails | Stage shows message + stderr tail; controls disabled |
| Storyboard fails or slow | Hover shows time-only tooltip; nothing else degrades |
| New encode started / job replaced | Media/convert endpoints return 409; viewer closes with a toast |
| Storyboard/convert processes at teardown | Killed via assets ctx; temp dir removed; stale sweeper covers crashes |

## Performance and fidelity trade-offs (accepted)

- Converted previews live under `os.TempDir()` (often tmpfs/RAM); bounded by
  the 1920-longest-edge cap, CRF quality targets, and the app's short-clip
  usage profile. Job-scoped cleanup keeps at most one job's previews alive.
- Two simultaneous software decodes (worst case two 4K AV1 streams) may drop
  frames on weak CPUs; browsers degrade gracefully and audio continues.
- A converted side carries mild generational artifacts (CRF 18 x264 / CRF 24
  VP9-realtime) on top of the encode being inspected — negligible relative to
  size-squeezed encodes, and only on sides the browser could not show at all.
- Direct playback is pixel-exact — better than today's 1280-capped PNGs.

## Testing

Go (rewrite):
- `TestCompletedCompressionOffersLargeSynchronizedComparison` inverts: assert
  `<video` elements, play/mute controls, sync/drift code, storyboard hover
  math; assert the per-frame machinery is **gone**
  (`/api/compare/frame`, `fetchCompareFrame`, hover fetch-per-move).
- `TestCompareFramesAreScopedAuthenticatedAndGenerated` is replaced by
  endpoint tests: auth (header + query token, constant-time), job scoping
  (404/409 paths), Range serving via `http.ServeContent`, convert lifecycle
  with a fake ffmpeg script (existing pattern in `server_test.go`), teardown
  on new job start deletes the dir and kills processes.

Go (new units): `compareMime` table, `planConversion` table (every row above,
incl. "neither profile" error), storyboard geometry (count/interval/tile
math, portrait + sub-1 s durations), progress parsing.

Manual (via the run skill): compress an H.264 sample (direct path: audio,
scrubbing, hover), an H.265 sample (convert path: progress → playback), drop
audio (mute disabled), start a new encode with the viewer open (409 → toast).

## Docs and packaging

- README: "Visual comparison" feature bullet rewritten — before/after playback
  with the compressed track's audio, native scrubbing, instant hover
  thumbnails; playable previews are generated only for codecs the browser
  cannot decode and are cleaned up automatically. Remove the
  "without browser codec limits, preview encodes, or temporary files" claim.
- `version` in `main.go` → `1.9.0`.

## Out of scope

- The drag-and-drop output-directory fix (separate task, in progress on a
  parallel branch of work).
- Hover thumbnails for the input side; playback of older jobs; export of
  comparison media.
