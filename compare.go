package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// compareMime assembles one side's three canPlayType inputs: the source
// container MIME deciding direct playback, plus per-codec target-container
// MIMEs that feed the conversion verdict chain.
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

// compareScaleFilter caps converted previews at a 1920px longest edge with
// even dimensions, matching the aspect-preserving idiom used elsewhere.
const compareScaleFilter = "scale=w='min(1920,iw)':h='min(1920,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"

// compareVerdicts are the client's canPlayType results for one side's mimes.
type compareVerdicts struct {
	Full  bool `json:"full"`
	Video bool `json:"video"`
	Audio bool `json:"audio"`
}

// compareProfiles report which candidate preview profiles — H.264+AAC MP4,
// VP9+Opus WebM — the client's browser can play.
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

// mp4CopyVideo/webmCopyVideo/mp4CopyAudio/webmCopyAudio say which codec
// copies are legal in each preview container.
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
// Callers hold no locks; App.mu guards the App.compare pointer. Assets found
// for a different job are necessarily stale — most likely a superseded
// job's own prepareCompareAssets losing a race with a newer job's start —
// so they are torn down here rather than overwritten in place; otherwise
// their temp directory and any in-flight FFmpeg render would leak.
func (a *App) ensureCompareAssets(job *Job) *compareAssets {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.compare != nil {
		if a.compare.job == job {
			return a.compare
		}
		stale := a.compare
		stale.cancel()
		stale.mu.Lock()
		staleDir := stale.dir
		stale.mu.Unlock()
		if staleDir != "" {
			_ = os.RemoveAll(staleDir)
		}
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
// viewer opens. This runs in its own goroutine and can lose a race with a
// newer job's start, so it first confirms job is still the current one
// before creating or touching any assets; a superseded job's prepare is a
// no-op instead of resurrecting assets a newer job's teardown just cleared.
func (a *App) prepareCompareAssets(job *Job) {
	a.mu.RLock()
	current := a.job
	a.mu.RUnlock()
	if current != job {
		return
	}
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

// teardownCompareAssets cancels and deletes the current job's compare
// assets, if any, so a stale preview directory and its background work never
// outlive the job that created them. It is always safe to call: most jobs
// never generate compare assets at all, so the common case is a no-op.
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
