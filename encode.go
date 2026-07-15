package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	minimumVideoBitrateKbps = 64
	maximumEncodeAttempts   = 8
)

var minimumAudioBitrateKbps = map[string]int{
	"aac": 16, "opus": 6, "vorbis": 48, "mp3": 32,
}

// downscaleLadder holds the output heights tried, top to bottom, when a
// hardware encoder cannot produce few enough bits at the current resolution.
var downscaleLadder = []int{720, 540, 360}

type EncoderInfo struct {
	ID       string `json:"id"`
	Codec    string `json:"codec"`
	Name     string `json:"name"`
	TwoPass  bool   `json:"twoPass"`
	Hardware bool   `json:"hardware"`
}

type EncodeRequest struct {
	Input            string `json:"input"`
	Output           string `json:"output"`
	TargetBytes      int64  `json:"targetBytes"`
	Container        string `json:"container"`
	VideoCodec       string `json:"videoCodec"`
	Encoder          string `json:"encoder"`
	Preset           string `json:"preset"`
	AudioCodec       string `json:"audioCodec"`
	AudioBitrateKbps int    `json:"audioBitrateKbps"`
	AudioChannels    string `json:"audioChannels"`
	TwoPass          bool   `json:"twoPass"`
	ResolutionHeight int    `json:"resolutionHeight"`
	Remux            bool   `json:"remux"`
	MuxAudio         bool   `json:"muxAudio"`
	VAAPIDevice      string `json:"-"`
	ScaleWidth       int    `json:"-"`
	ScaleHeight      int    `json:"-"`
}

// mapProbeCodec normalizes an ffprobe codec name onto the app's codec keys;
// unknown codecs return "".
func mapProbeCodec(name string) string {
	name = strings.ToLower(name)
	switch {
	case strings.Contains(name, "av1"):
		return "av1"
	case strings.Contains(name, "hevc"), strings.Contains(name, "265"):
		return "h265"
	case strings.Contains(name, "h264"), strings.Contains(name, "avc"):
		return "h264"
	case strings.Contains(name, "vvc"), strings.Contains(name, "266"):
		return "h266"
	case strings.Contains(name, "vp9"):
		return "vp9"
	default:
		return ""
	}
}

// allowedResolutionHeights are the selectable output heights; 0 means auto
// (source resolution, downscaled only if a hardware floor demands it).
var allowedResolutionHeights = map[int]bool{0: true, 2160: true, 1440: true, 1080: true, 720: true, 540: true, 480: true, 360: true}

type JobSnapshot struct {
	State            string  `json:"state"`
	Phase            string  `json:"phase"`
	Message          string  `json:"message"`
	Progress         float64 `json:"progress"`
	Pass             int     `json:"pass"`
	Passes           int     `json:"passes"`
	Attempt          int     `json:"attempt"`
	ElapsedSeconds   float64 `json:"elapsedSeconds"`
	RemainingSeconds float64 `json:"remainingSeconds"`
	EncodedBytes     int64   `json:"encodedBytes"`
	TargetBytes      int64   `json:"targetBytes"`
	VideoBitrateKbps int     `json:"videoBitrateKbps"`
	Speed            string  `json:"speed"`
	Output           string  `json:"output"`
	Error            string  `json:"error,omitempty"`
}

type Job struct {
	request EncodeRequest
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time

	mu     sync.RWMutex
	status JobSnapshot
}

func newJob(request EncodeRequest) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	return &Job{
		request: request,
		ctx:     ctx,
		cancel:  cancel,
		started: time.Now(),
		status: JobSnapshot{
			State:       "queued",
			Phase:       "Preparing",
			Message:     "Reading video metadata…",
			TargetBytes: request.TargetBytes,
			Output:      request.Output,
		},
	}
}

func (j *Job) snapshot() JobSnapshot {
	j.mu.RLock()
	defer j.mu.RUnlock()
	copy := j.status
	copy.ElapsedSeconds = time.Since(j.started).Seconds()
	return copy
}

func (j *Job) isTerminal() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.status.State == "completed" || j.status.State == "failed" || j.status.State == "canceled"
}

func (j *Job) set(update func(*JobSnapshot)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	update(&j.status)
}

func (j *Job) fail(err error) {
	state := "failed"
	message := "Compression failed"
	if errors.Is(err, context.Canceled) || errors.Is(j.ctx.Err(), context.Canceled) {
		state = "canceled"
		message = "Compression canceled"
	}
	j.set(func(status *JobSnapshot) {
		status.State = state
		status.Phase = message
		status.Message = message
		status.Error = ""
		if state == "failed" {
			status.Error = cleanFFmpegError(err.Error())
		}
		status.RemainingSeconds = 0
	})
}

func (j *Job) run(ffmpeg, ffprobe string) {
	if err := j.runEncode(ffmpeg, ffprobe); err != nil {
		j.fail(err)
	}
}

func (j *Job) runEncode(ffmpeg, ffprobe string) error {
	info, err := probeVideo(j.ctx, ffprobe, j.request.Input)
	if err != nil {
		return err
	}
	if err := j.validateWithProbe(info); err != nil {
		return err
	}
	if j.request.Remux {
		return j.runRemux(ffmpeg, info)
	}

	encoder, ok := supportedEncoder(j.request.Encoder)
	if !ok || encoder.Codec != j.request.VideoCodec {
		return errors.New("the selected video encoder is not compatible with the selected codec")
	}
	// An explicit resolution is fixed for the whole job; auto (0) starts at
	// the source resolution and may step down if a hardware floor demands it.
	autoResolution := j.request.ResolutionHeight == 0
	if !autoResolution {
		if width, height := scaleDimensions(info.Width, info.Height, j.request.ResolutionHeight); width > 0 {
			j.request.ScaleWidth, j.request.ScaleHeight = width, height
		}
	}
	useTwoPass := j.request.TwoPass && encoder.TwoPass
	passes := 1
	if useTwoPass {
		passes = 2
	}

	videoKbps, err := calculateVideoBitrate(j.request, info)
	if err != nil {
		return err
	}
	if encoder.Hardware {
		// Hardware rate control tends to land a few percent above the request; asking
		// for slightly less makes most encodes fit on the first attempt.
		videoKbps = hardwareSafeBitrate(videoKbps)
	}

	outputDir := filepath.Dir(j.request.Output)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output folder: %w", err)
	}
	tempDir, err := os.MkdirTemp(outputDir, ".exactsize-work-")
	if err != nil {
		return fmt.Errorf("create temporary work folder: %w", err)
	}
	defer os.RemoveAll(tempDir)
	tempOutput := filepath.Join(tempDir, "output."+containerExtension(j.request.Container))
	passLog := filepath.Join(tempDir, "pass")

	j.set(func(status *JobSnapshot) {
		status.State = "running"
		status.Passes = passes
		status.VideoBitrateKbps = videoKbps
	})

	originalKbps := videoKbps

	for attempt := 1; attempt <= maximumEncodeAttempts; attempt++ {
		if err := j.ctx.Err(); err != nil {
			return err
		}
		_ = os.Remove(tempOutput)
		removePassLogs(passLog)

		j.set(func(status *JobSnapshot) {
			status.Attempt = attempt
			status.VideoBitrateKbps = videoKbps
			status.EncodedBytes = 0
			status.Progress = 0
			status.Speed = ""
			status.Error = ""
			if attempt > 1 {
				status.Message = fmt.Sprintf("Tightening the size target (attempt %d)…", attempt)
			}
		})

		if useTwoPass {
			firstPass := buildFFmpegArgs(j.request, info, videoKbps, tempOutput, passLog, 1, true)
			if err := j.runFFmpeg(ffmpeg, firstPass, info.Duration, info.FPS, 1, 2, attempt, tempOutput); err != nil {
				return err
			}
			secondPass := buildFFmpegArgs(j.request, info, videoKbps, tempOutput, passLog, 2, true)
			if err := j.runFFmpeg(ffmpeg, secondPass, info.Duration, info.FPS, 2, 2, attempt, tempOutput); err != nil {
				return err
			}
		} else {
			args := buildFFmpegArgs(j.request, info, videoKbps, tempOutput, passLog, 1, false)
			if err := j.runFFmpeg(ffmpeg, args, info.Duration, info.FPS, 1, 1, attempt, tempOutput); err != nil {
				return err
			}
		}

		j.set(func(status *JobSnapshot) {
			status.Phase = "Verifying"
			status.Message = "Checking the exact output size…"
			status.Progress = 99
		})
		stat, err := os.Stat(tempOutput)
		if err != nil {
			return errors.New("FFmpeg finished without creating an output file")
		}
		actualBytes := stat.Size()
		j.set(func(status *JobSnapshot) { status.EncodedBytes = actualBytes })

		if actualBytes <= j.request.TargetBytes {
			if err := publishOutput(tempOutput, j.request.Output); err != nil {
				return err
			}
			message := "Video compressed successfully"
			if j.request.ScaleHeight > 0 && autoResolution {
				message = fmt.Sprintf("Video compressed successfully at %d×%d (downscaled to fit the target)", j.request.ScaleWidth, j.request.ScaleHeight)
			} else if j.request.ScaleHeight > 0 {
				message = fmt.Sprintf("Video compressed successfully at %d×%d", j.request.ScaleWidth, j.request.ScaleHeight)
			}
			j.set(func(status *JobSnapshot) {
				status.State = "completed"
				status.Phase = "Complete"
				status.Message = message
				status.Progress = 100
				status.RemainingSeconds = 0
				status.EncodedBytes = actualBytes
			})
			return nil
		}

		actualVideoKbps := measuredVideoKbps(actualBytes, info.Duration, info.AudioTracks, j.request.AudioCodec, j.request.AudioBitrateKbps)
		availableVideoKbps := 0
		if breakdown, probeErr := probeOutputBreakdown(j.ctx, ffprobe, tempOutput); probeErr != nil {
			if j.ctx.Err() != nil {
				return j.ctx.Err()
			}
		} else {
			actualVideoKbps = streamBitrateKbps(breakdown.VideoBytes, info.Duration)
			availableVideoKbps = outputVideoBudgetKbps(j.request.TargetBytes, info.Duration, breakdown)
			if availableVideoKbps < minimumVideoBitrateKbps {
				return errors.New("the target is too small after measured audio and container overhead")
			}
		}

		if attempt == maximumEncodeAttempts {
			if encoder.Hardware {
				return fmt.Errorf("could not bring the output below the strict target after %d attempts; a software encoder, a lower resolution, or a different video codec may reach targets the GPU cannot", maximumEncodeAttempts)
			}
			return fmt.Errorf("could not bring the output below the strict target after %d attempts", maximumEncodeAttempts)
		}

		if encoder.Hardware {
			// Hardware encoders miss low targets additively: driver quality
			// floors (keyframes especially) add a roughly constant bitrate on
			// top of the request. Subtracting the measured excess converges
			// in one correction where a ratio cut needs several. When the
			// excess eats most of the budget, exhaust the minimum bitrate
			// before allowing Auto resolution to step down.
			correctionBudgetKbps := originalKbps
			if availableVideoKbps > 0 {
				correctionBudgetKbps = hardwareSafeBitrate(availableVideoKbps)
			}
			retryKbps, hopeless := hardwareCorrection(correctionBudgetKbps, videoKbps, actualVideoKbps)
			if likelyAV1VAAPIBitrateFloor(j.request, info, videoKbps, actualVideoKbps, correctionBudgetKbps) {
				hopeless = true
			}
			if hopeless {
				if minimumKbps, ok := minimumBitrateRetry(videoKbps); ok {
					videoKbps = minimumKbps
					j.set(func(status *JobSnapshot) {
						status.Message = "Trying the minimum video bitrate before reducing resolution…"
					})
					continue
				}
				if !autoResolution {
					return errors.New("the GPU encoder cannot reach this target at the selected resolution; lower the resolution, switch to a software encoder, or raise the target")
				}
				width, height, ok := floorAwareDownscale(info.Width, info.Height, j.request.ScaleHeight, actualVideoKbps, correctionBudgetKbps)
				if !ok {
					return errors.New("the GPU encoder cannot reach this target even at reduced resolution; switch to a software encoder, try a different video codec, or raise the target")
				}
				j.request.ScaleWidth, j.request.ScaleHeight = width, height
				videoKbps = correctionBudgetKbps
				j.set(func(status *JobSnapshot) {
					status.Message = fmt.Sprintf("The GPU encoder hit its minimum bitrate; retrying at %d×%d…", width, height)
				})
				continue
			}
			if retryKbps > 0 {
				videoKbps = retryKbps
				j.set(func(status *JobSnapshot) {
					status.Message = "Compensating for the encoder's fixed overhead…"
				})
				continue
			}
		}

		if availableVideoKbps > 0 && actualVideoKbps > 0 {
			correctionBudgetKbps := availableVideoKbps
			if encoder.Hardware {
				correctionBudgetKbps = hardwareSafeBitrate(correctionBudgetKbps)
			}
			videoKbps = proportionalVideoCorrection(videoKbps, actualVideoKbps, correctionBudgetKbps)
			if videoKbps < minimumVideoBitrateKbps {
				return errors.New("the target is too small for this duration and measured non-video overhead")
			}
			continue
		}

		// Scale by the measured overshoot and leave an extra 0.8%% margin.
		ratio := float64(j.request.TargetBytes) / float64(actualBytes)
		videoKbps = int(math.Floor(float64(videoKbps) * ratio * 0.992))
		if videoKbps < minimumVideoBitrateKbps {
			return errors.New("the target is too small for this duration and audio bitrate")
		}
	}

	return errors.New("compression ended unexpectedly")
}

// mapProbeAudioCodec normalizes an ffprobe audio codec name onto the app's
// audio keys; unknown codecs return "".
func mapProbeAudioCodec(name string) string {
	switch strings.ToLower(name) {
	case "aac":
		return "aac"
	case "opus":
		return "opus"
	case "vorbis":
		return "vorbis"
	case "mp3":
		return "mp3"
	default:
		return ""
	}
}

// remuxCompatibility reports whether the probed streams can be carried into
// the container unchanged. Codecs the app does not know are allowed into MKV
// only, which holds nearly anything.
func remuxCompatibility(info VideoInfo, container string, copyAudio bool) error {
	if key := mapProbeCodec(info.VideoCodec); key != "" {
		if !containerSupportsCodec(container, key) {
			return fmt.Errorf("a %s video stream cannot be remuxed into %s; pick a compatible container", strings.ToUpper(info.VideoCodec), strings.ToUpper(container))
		}
	} else if container != "mkv" {
		return fmt.Errorf("the source video codec (%s) is only safe to remux into MKV", info.VideoCodec)
	}
	if copyAudio && info.AudioTracks > 0 {
		if key := mapProbeAudioCodec(info.AudioCodec); key != "" {
			if !containerSupportsAudio(container, key) {
				return fmt.Errorf("%s audio cannot be remuxed into %s; pick a compatible container such as MKV", strings.ToUpper(info.AudioCodec), strings.ToUpper(container))
			}
		} else if container != "mkv" {
			return fmt.Errorf("the source audio codec (%s) is only safe to remux into MKV", info.AudioCodec)
		}
	}
	return nil
}

// runRemux copies every video and audio stream into the selected container
// without re-encoding. The strict size target does not apply; the output is
// published once the muxer finishes.
func (j *Job) runRemux(ffmpeg string, info VideoInfo) error {
	container := j.request.Container
	if err := remuxCompatibility(info, container, !j.request.MuxAudio); err != nil {
		return err
	}

	outputDir := filepath.Dir(j.request.Output)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output folder: %w", err)
	}
	tempDir, err := os.MkdirTemp(outputDir, ".exactsize-work-")
	if err != nil {
		return fmt.Errorf("create temporary work folder: %w", err)
	}
	defer os.RemoveAll(tempDir)
	tempOutput := filepath.Join(tempDir, "output."+containerExtension(container))

	j.set(func(status *JobSnapshot) {
		status.State = "running"
		status.Passes = 1
		status.Attempt = 1
		status.Phase = "Remuxing"
		status.Message = "Copying streams into the new container…"
	})

	args := []string{
		"-hide_banner", "-y", "-nostdin", "-loglevel", "error", "-stats_period", "0.25",
		"-i", j.request.Input,
		"-map", "0:v:0", "-map", "0:a?",
		"-c:v", "copy",
	}
	if j.request.MuxAudio {
		args = append(args, audioEncoderArgs(j.request)...)
	} else {
		args = append(args, "-c:a", "copy")
	}
	args = append(args,
		"-sn", "-dn",
		"-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096",
	)
	if container == "mp4" || container == "mov" {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, "-progress", "pipe:1", "-nostats", "-f", muxerName(container), tempOutput)

	if err := j.runFFmpeg(ffmpeg, args, info.Duration, info.FPS, 1, 1, 1, tempOutput); err != nil {
		return err
	}
	stat, err := os.Stat(tempOutput)
	if err != nil {
		return errors.New("FFmpeg finished without creating an output file")
	}
	if err := publishOutput(tempOutput, j.request.Output); err != nil {
		return err
	}
	message := fmt.Sprintf("Remuxed losslessly to %s", strings.ToUpper(container))
	if j.request.MuxAudio {
		message = fmt.Sprintf("Muxed to %s (video copied losslessly, audio re-encoded)", strings.ToUpper(container))
	}
	j.set(func(status *JobSnapshot) {
		status.State = "completed"
		status.Phase = "Complete"
		status.Message = message
		status.Progress = 100
		status.RemainingSeconds = 0
		status.EncodedBytes = stat.Size()
	})
	return nil
}

func (j *Job) runFFmpeg(ffmpeg string, args []string, duration, fps float64, pass, passes, attempt int, output string) error {
	phase := "Encoding"
	if passes == 2 {
		phase = fmt.Sprintf("Encoding pass %d of 2", pass)
	}
	j.set(func(status *JobSnapshot) {
		status.State = "running"
		status.Phase = phase
		status.Message = phase + "…"
		status.Pass = pass
		status.Passes = passes
	})

	command := exec.CommandContext(j.ctx, ffmpeg, args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start FFmpeg: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), "=")
		if !found {
			continue
		}
		if seconds, ok := progressSeconds(key, value, fps); ok {
			j.updateProgress(seconds, duration, pass, passes, attempt, output)
		}
		switch key {
		case "speed":
			j.set(func(status *JobSnapshot) { status.Speed = strings.TrimSpace(value) })
		}
	}
	if err := scanner.Err(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	if err := command.Wait(); err != nil {
		if j.ctx.Err() != nil {
			return j.ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("FFmpeg: %s", detail)
	}
	return nil
}

func (j *Job) updateProgress(encodedSeconds, duration float64, pass, passes, attempt int, output string) {
	if duration <= 0 || encodedSeconds < 0 || math.IsNaN(encodedSeconds) || math.IsInf(encodedSeconds, 0) {
		return
	}
	passFraction := math.Max(0, math.Min(1, encodedSeconds/duration))
	overall := passFraction * 98
	if passes == 2 {
		if pass == 1 {
			overall = passFraction * 45
		} else {
			overall = 45 + passFraction*53
		}
	}
	if attempt > 1 {
		// A retry is exceptional; keep the bar honest without jumping backward to zero.
		overall = math.Max(4, overall)
	}
	encodedBytes := int64(0)
	if stat, err := os.Stat(output); err == nil {
		encodedBytes = stat.Size()
	}
	elapsed := time.Since(j.started).Seconds()
	remaining := float64(0)
	if overall > 0.5 {
		remaining = math.Max(0, elapsed*(100-overall)/overall)
	}
	j.set(func(status *JobSnapshot) {
		// FFmpeg can report a delayed timestamp after a newer frame count,
		// especially during a null-output first pass. Never move backward.
		if overall > status.Progress {
			status.Progress = overall
		}
		status.EncodedBytes = encodedBytes
		status.ElapsedSeconds = elapsed
		status.RemainingSeconds = remaining
	})
}

func buildFFmpegArgs(request EncodeRequest, info VideoInfo, videoKbps int, output, passLog string, pass int, twoPass bool) []string {
	args := []string{
		"-hide_banner", "-y", "-nostdin", "-loglevel", "error", "-stats_period", "0.25",
	}
	if isVAAPIEncoder(request.Encoder) && request.VAAPIDevice != "" {
		args = append(args, "-vaapi_device", request.VAAPIDevice)
	}
	args = append(args,
		"-i", request.Input,
		"-map", "0:v:0",
	)
	if !twoPass || pass == 2 {
		args = append(args, "-map", "0:a?")
	}
	args = append(args, videoEncoderArgs(request, info, videoKbps)...)
	if twoPass {
		args = append(args, "-pass", strconv.Itoa(pass), "-passlogfile", passLog)
	}

	if twoPass && pass == 1 {
		args = append(args,
			"-an", "-sn", "-dn",
			"-fps_mode", "passthrough",
			"-progress", "pipe:1", "-nostats",
			"-f", "null", "/dev/null",
		)
		return args
	}

	args = append(args, audioEncoderArgs(request)...)
	args = append(args,
		"-sn", "-dn",
		"-map_metadata", "0", "-map_chapters", "0",
		"-max_muxing_queue_size", "4096",
		"-fps_mode", "passthrough",
	)
	if request.Container == "mp4" || request.Container == "mov" {
		args = append(args, "-movflags", "+faststart")
		if request.VideoCodec == "h265" && request.Encoder == "libx265" {
			// hvc1 (preferred by Apple players) moves the parameter sets
			// out-of-band, and the muxer strips them from the stream. Only
			// libx265 reliably provides the required global extradata;
			// hevc_vaapi does not, which yields an undecodable file. Other
			// encoders keep the hev1 default with in-band parameter sets.
			args = append(args, "-tag:v", "hvc1")
		}
	}
	args = append(args,
		"-progress", "pipe:1", "-nostats",
		"-f", muxerName(request.Container), output,
	)
	return args
}

func videoEncoderArgs(request EncodeRequest, info VideoInfo, bitrateKbps int) []string {
	args := []string{"-c:v", request.Encoder, "-b:v", fmt.Sprintf("%dk", bitrateKbps)}
	quality := strings.ToLower(request.Preset)
	pick := func(fastest, fast, balanced, best string) string {
		value := map[string]string{"fastest": fastest, "fast": fast, "balanced": balanced, "quality": best}[quality]
		if value == "" {
			return balanced
		}
		return value
	}
	outputHeight := info.Height
	if request.ScaleHeight > 0 && request.ScaleHeight < outputHeight {
		outputHeight = request.ScaleHeight
	}
	threads := strconv.Itoa(encoderThreads())
	switch {
	case request.Encoder == "libx264" || request.Encoder == "libx265":
		args = append(args, "-preset", pick("veryfast", "fast", "medium", "slow"))
	case request.Encoder == "libvvenc":
		args = append(args, "-preset", pick("faster", "fast", "medium", "slow"))
	case request.Encoder == "libaom-av1":
		// libaom threads poorly without tiles; size them to the output and
		// widen them on the fastest preset.
		args = append(args, "-cpu-used", pick("8", "8", "6", "4"), "-row-mt", "1", "-threads", threads)
		if tiles := aomTiles(outputHeight, quality == "fastest"); tiles != "" {
			args = append(args, "-tiles", tiles)
		}
	case request.Encoder == "libvpx-vp9":
		tileColumns := "2"
		if quality == "fastest" && outputHeight >= 1080 {
			tileColumns = "3"
		}
		args = append(args, "-deadline", "good", "-cpu-used", pick("5", "4", "2", "1"), "-row-mt", "1", "-tile-columns", tileColumns, "-frame-parallel", "1", "-threads", threads)
	case strings.HasSuffix(request.Encoder, "_nvenc"):
		args = append(args, "-preset", pick("p1", "p3", "p5", "p7"))
	case strings.HasSuffix(request.Encoder, "_qsv"):
		args = append(args, "-preset", pick("veryfast", "veryfast", "medium", "veryslow"))
	case strings.HasSuffix(request.Encoder, "_amf"):
		args = append(args, "-quality", pick("speed", "speed", "balanced", "quality"))
	}
	if supported, _ := supportedEncoder(request.Encoder); supported.Hardware {
		// Hardware rate control drifts further from the requested average than
		// software encoders; a tight cap keeps the verify-and-retry loop bounded.
		args = append(args, "-maxrate", fmt.Sprintf("%dk", bitrateKbps), "-bufsize", fmt.Sprintf("%dk", bitrateKbps*2))
	}

	tenBitSource := strings.Contains(info.PixelFormat, "10") || strings.Contains(info.PixelFormat, "12")
	switch {
	case isVAAPIEncoder(request.Encoder):
		if request.Encoder == "h264_vaapi" {
			// AMD VCN's H.264 B-frames are encoded far worse than the P-frames
			// around them, which shows as quality pulsing (judder) on motion.
			args = append(args, "-bf", "0")
		}
		if request.Encoder == "h264_vaapi" || request.Encoder == "hevc_vaapi" || request.Encoder == "av1_vaapi" {
			// The VAAPI default GOP of 120 frames is expensive: the driver
			// holds keyframes above a quality floor, and each one costs a
			// fixed chunk that rate control cannot reclaim. 250 matches the
			// software encoders' default cadence.
			args = append(args, "-g", "250")
		}
		// VAAPI encodes from GPU surfaces, so upload replaces -pix_fmt.
		surface := "nv12"
		if tenBitSource && request.VideoCodec != "h264" {
			surface = "p010"
		}
		filter := "format=" + surface + ",hwupload"
		if request.ScaleHeight > 0 {
			filter += fmt.Sprintf(",scale_vaapi=%d:%d", request.ScaleWidth, request.ScaleHeight)
		}
		args = append(args, "-vf", filter)
	case request.Encoder == "libvvenc":
		// vvenc encodes Main 10 only.
		args = appendScale(args, request)
		args = append(args, "-pix_fmt", "yuv420p10le")
	case strings.HasSuffix(request.Encoder, "_nvenc"), strings.HasSuffix(request.Encoder, "_qsv"), strings.HasSuffix(request.Encoder, "_amf"):
		args = appendScale(args, request)
		if tenBitSource && request.VideoCodec != "h264" {
			args = append(args, "-pix_fmt", "p010le")
		} else {
			args = append(args, "-pix_fmt", "nv12")
		}
	case request.VideoCodec == "h264":
		args = appendScale(args, request)
		args = append(args, "-pix_fmt", "yuv420p")
	case tenBitSource:
		args = appendScale(args, request)
		args = append(args, "-pix_fmt", "yuv420p10le")
	default:
		args = appendScale(args, request)
		args = append(args, "-pix_fmt", "yuv420p")
	}
	return args
}

func appendScale(args []string, request EncodeRequest) []string {
	if request.ScaleHeight <= 0 {
		return args
	}
	return append(args, "-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", request.ScaleWidth, request.ScaleHeight))
}

// scaleDimensions maps a target height onto the source aspect ratio with even
// dimensions. It returns zeros when no scaling is needed or possible.
func scaleDimensions(sourceWidth, sourceHeight, targetHeight int) (int, int) {
	if targetHeight <= 0 || sourceWidth <= 0 || sourceHeight <= 0 || targetHeight >= sourceHeight {
		return 0, 0
	}
	width := sourceWidth * targetHeight / sourceHeight
	width -= width % 2
	height := targetHeight - targetHeight%2
	if width < 2 || height < 2 {
		return 0, 0
	}
	return width, height
}

// encoderThreads caps the thread request; encoders gain little beyond 16 and
// higher values only add memory pressure.
func encoderThreads() int {
	threads := runtime.NumCPU()
	if threads > 16 {
		return 16
	}
	if threads < 1 {
		return 1
	}
	return threads
}

// aomTiles picks a libaom tile grid for the output height: measured on
// 1080p60 content, 2x2 tiles with row multithreading cut encode time by a
// quarter. Wide grids trade a little compression for more parallelism.
func aomTiles(height int, wide bool) string {
	switch {
	case height >= 1080:
		if wide {
			return "4x2"
		}
		return "2x2"
	case height >= 720:
		if wide {
			return "4x1"
		}
		return "2x1"
	default:
		if wide {
			return "2x1"
		}
		return ""
	}
}

// measuredVideoKbps estimates the video bitrate of a finished attempt by
// subtracting the audio budget from the measured file size.
func measuredVideoKbps(totalBytes int64, duration float64, audioTracks int, audioCodec string, audioKbps int) int {
	if duration <= 0 {
		return 0
	}
	audioBits := 0.0
	if audioCodec != "none" && audioTracks > 0 {
		audioBits = duration * float64(audioKbps*1000*audioTracks)
	}
	videoBits := float64(totalBytes)*8 - audioBits
	if videoBits <= 0 {
		return 0
	}
	return int(videoBits / duration / 1000)
}

func streamBitrateKbps(streamBytes int64, duration float64) int {
	if streamBytes <= 0 || duration <= 0 {
		return 0
	}
	return int(math.Floor(float64(streamBytes) * 8 / duration / 1000))
}

// outputVideoBudgetKbps uses the measured audio payload and mux bytes from a
// failed attempt to determine how much of the target can actually be video.
func outputVideoBudgetKbps(targetBytes int64, duration float64, breakdown OutputBreakdown) int {
	if targetBytes <= 0 || duration <= 0 {
		return 0
	}
	nonVideoBytes := breakdown.AudioBytes + breakdown.OtherBytes + breakdown.MuxBytes
	if nonVideoBytes >= targetBytes {
		return 0
	}
	return streamBitrateKbps(targetBytes-nonVideoBytes, duration)
}

func hardwareSafeBitrate(videoKbps int) int {
	videoKbps = videoKbps * 96 / 100
	if videoKbps < minimumVideoBitrateKbps {
		return minimumVideoBitrateKbps
	}
	return videoKbps
}

func proportionalVideoCorrection(requestedKbps, actualKbps, budgetKbps int) int {
	if requestedKbps <= 0 || actualKbps <= 0 || budgetKbps <= 0 {
		return 0
	}
	return int(math.Floor(float64(requestedKbps) * float64(budgetKbps) / float64(actualKbps) * 0.992))
}

// likelyAV1VAAPIBitrateFloor recognizes the severe low-bits-per-pixel miss
// seen when AV1 VAAPI reaches its quantizer floor. A normal correction cannot
// reclaim this much bitrate, so the correction path should try the minimum
// bitrate next and only then allow Auto resolution to downscale.
func likelyAV1VAAPIBitrateFloor(request EncodeRequest, info VideoInfo, requestedKbps, actualKbps, budgetKbps int) bool {
	if request.Encoder != "av1_vaapi" || requestedKbps <= 0 || actualKbps <= budgetKbps {
		return false
	}
	if int64(actualKbps)*100 < int64(requestedKbps)*135 {
		return false
	}
	width, height := info.Width, info.Height
	if request.ScaleWidth > 0 && request.ScaleHeight > 0 {
		width, height = request.ScaleWidth, request.ScaleHeight
	}
	fps := info.FPS
	if width <= 0 || height <= 0 || fps <= 0 {
		return false
	}
	bitsPerPixelFrame := float64(requestedKbps*1000) / float64(width*height) / fps
	return bitsPerPixelFrame <= 0.012
}

// minimumBitrateRetry exhausts the current resolution's bitrate option before
// a hardware encode is allowed to downscale. Once the minimum has already
// failed, the caller can safely proceed to the resolution fallback.
func minimumBitrateRetry(requestedKbps int) (int, bool) {
	if requestedKbps <= minimumVideoBitrateKbps {
		return 0, false
	}
	return minimumVideoBitrateKbps, true
}

// hardwareCorrection turns an oversized hardware attempt into the next move.
// It returns a corrected bitrate request (measured excess subtracted with a
// 15% margin, since the excess grows slightly as requests shrink), or
// hopeless=true when the excess consumes over 60% of the budget and an
// ordinary correction is no longer useful. The caller then tries the minimum
// bitrate before considering a lower resolution. Both zero values mean the
// overshoot was not additive; the caller falls back to proportional correction.
func hardwareCorrection(budgetKbps, requestedKbps, actualKbps int) (int, bool) {
	excess := actualKbps - requestedKbps
	if excess <= 0 {
		return 0, false
	}
	corrected := budgetKbps - excess*23/20
	if corrected < minimumVideoBitrateKbps || corrected*10 < budgetKbps*4 {
		return 0, true
	}
	return corrected, false
}

// floorAwareDownscale picks the highest ladder resolution whose predicted
// bitrate floor fits the budget, scaling the observed floor by pixel count.
func floorAwareDownscale(sourceWidth, sourceHeight, currentHeight, actualKbps, budgetKbps int) (int, int, bool) {
	effective := sourceHeight
	if currentHeight > 0 && currentHeight < effective {
		effective = currentHeight
	}
	if effective <= 0 || actualKbps <= 0 {
		return 0, 0, false
	}
	for _, height := range downscaleLadder {
		if height >= effective {
			continue
		}
		// Floors do not scale purely with pixel count: measured VAAPI floors
		// (734/373/242/166 kbps at 1080p/720p/540p/360p) fit a fixed
		// per-frame share of ~15% plus a pixel-proportional share.
		pixelRatio := float64(height*height) / float64(effective*effective)
		predicted := float64(actualKbps) * (0.15 + 0.85*pixelRatio)
		if predicted > float64(budgetKbps)*0.9 {
			continue
		}
		if width, scaled := scaleDimensions(sourceWidth, sourceHeight, height); width > 0 {
			return width, scaled, true
		}
	}
	return 0, 0, false
}

func audioEncoderArgs(request EncodeRequest) []string {
	if request.AudioCodec == "none" {
		return []string{"-an"}
	}
	encoder := map[string]string{
		"aac": "aac", "opus": "libopus", "vorbis": "libvorbis", "mp3": "libmp3lame",
	}[request.AudioCodec]
	args := []string{"-c:a", encoder, "-b:a", fmt.Sprintf("%dk", request.AudioBitrateKbps)}
	switch request.AudioChannels {
	case "mono":
		args = append(args, "-ac", "1")
	case "stereo":
		args = append(args, "-ac", "2")
	}
	return args
}

func calculateVideoBitrate(request EncodeRequest, info VideoInfo) (int, error) {
	if request.TargetBytes <= 0 || info.Duration <= 0 {
		return 0, errors.New("target size and duration must be greater than zero")
	}
	reserve := estimatedMuxOverheadBytes(request.Container, info, request.AudioCodec)
	usableBits := float64(request.TargetBytes-reserve) * 8
	if usableBits <= 0 {
		return 0, errors.New("the target size is too small")
	}
	if request.AudioCodec != "none" && info.AudioTracks > 0 {
		usableBits -= info.Duration * float64(request.AudioBitrateKbps*1000*info.AudioTracks)
	}
	videoKbps := int(math.Floor(usableBits / info.Duration / 1000))
	if videoKbps < minimumVideoBitrateKbps {
		return 0, fmt.Errorf("target is too small: the video would receive only %d kbps after audio and container overhead", videoKbps)
	}
	return videoKbps, nil
}

// estimatedMuxOverheadBytes models the structures that scale with packet and
// stream counts (MP4 sample tables, Matroska/WebM blocks and indexes), plus a
// fixed allowance for headers, metadata, and chapters. The 15% guard covers
// timestamp layout differences without wasting a fixed percentage of large
// targets or under-reserving long, high-frame-rate files.
func estimatedMuxOverheadBytes(container string, info VideoInfo, audioCodec string) int64 {
	duration := info.Duration
	if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return 64 * 1024
	}
	fps := info.FPS
	if fps <= 0 || math.IsNaN(fps) || math.IsInf(fps, 0) {
		fps = 30
	}

	videoPackets := int64(math.Ceil(duration * fps))
	audioTracks := info.AudioTracks
	if audioCodec == "none" || audioTracks < 0 {
		audioTracks = 0
	}
	audioPackets := int64(0)
	if audioTracks > 0 {
		perTrack := int64(math.Ceil(duration * audioPacketRate(audioCodec, info.AudioSampleRate)))
		audioPackets = perTrack * int64(audioTracks)
	}

	baseBytes, streamBytes, packetBytes := int64(8*1024), int64(3*1024), int64(8)
	switch container {
	case "mp4", "mov":
		baseBytes, streamBytes, packetBytes = 12*1024, 4*1024, 10
	case "webm", "mkv":
		baseBytes, streamBytes, packetBytes = 6*1024, 2*1024, 7
	}
	streams := int64(1 + audioTracks)
	rawBytes := baseBytes + streams*streamBytes + (videoPackets+audioPackets)*packetBytes
	estimate := int64(math.Ceil(float64(rawBytes) * 1.15))
	if estimate < 64*1024 {
		return 64 * 1024
	}
	return estimate
}

func audioPacketRate(codec string, sampleRate int) float64 {
	if sampleRate <= 0 {
		sampleRate = 48_000
	}
	switch codec {
	case "opus":
		return 50 // libopus defaults to 20 ms packets.
	case "aac":
		return float64(sampleRate) / 1024
	case "mp3":
		return float64(sampleRate) / 1152
	case "vorbis":
		// Vorbis blocks vary; the smaller block size is the safer estimate.
		return float64(sampleRate) / 512
	default:
		return float64(sampleRate) / 1024
	}
}

func validateEncodeRequest(request EncodeRequest) error {
	request.Input = strings.TrimSpace(request.Input)
	request.Output = strings.TrimSpace(request.Output)
	if request.Input == "" {
		return errors.New("select an input video")
	}
	if request.Output == "" {
		return errors.New("choose an output destination")
	}
	if filepath.Clean(request.Input) == filepath.Clean(request.Output) {
		return errors.New("the output path must be different from the input video")
	}
	if request.Remux {
		// A remux copies streams as they are; only the container matters.
		// With MuxAudio the audio is re-encoded, so those fields apply.
		valid := map[string]bool{"mp4": true, "mkv": true, "webm": true, "mov": true}
		if !valid[request.Container] {
			return errors.New("select a valid output container")
		}
		if request.MuxAudio {
			if !containerSupportsAudio(request.Container, request.AudioCodec) {
				return errors.New("the selected container does not support that audio codec")
			}
			if err := validateAudioBitrate(request.AudioCodec, request.AudioBitrateKbps); err != nil {
				return err
			}
		}
		return nil
	}
	if request.TargetBytes < 256*1024 {
		return errors.New("the target must be at least 256 KB")
	}
	if !containerSupportsCodec(request.Container, request.VideoCodec) {
		return errors.New("the selected container does not support that video codec")
	}
	encoder, ok := supportedEncoder(request.Encoder)
	if !ok || encoder.Codec != request.VideoCodec {
		return errors.New("select a compatible video encoder")
	}
	if !containerSupportsAudio(request.Container, request.AudioCodec) {
		return errors.New("the selected container does not support that audio codec")
	}
	if err := validateAudioBitrate(request.AudioCodec, request.AudioBitrateKbps); err != nil {
		return err
	}
	if !allowedResolutionHeights[request.ResolutionHeight] {
		return errors.New("select a supported output resolution")
	}
	return nil
}

func validateAudioBitrate(codec string, bitrateKbps int) error {
	if codec == "none" {
		return nil
	}
	minimum := minimumAudioBitrateKbps[codec]
	if minimum == 0 {
		minimum = 16
	}
	if bitrateKbps < minimum || bitrateKbps > 1024 {
		return fmt.Errorf("%s audio bitrate must be between %d and 1024 kbps", strings.ToUpper(codec), minimum)
	}
	return nil
}

func (j *Job) validateWithProbe(info VideoInfo) error {
	if info.Duration <= 0 {
		return errors.New("the input video has no usable duration")
	}
	if stat, err := os.Stat(filepath.Dir(j.request.Output)); err == nil && !stat.IsDir() {
		return errors.New("the selected output folder is invalid")
	}
	return nil
}

func supportedEncoder(id string) (EncoderInfo, bool) {
	for _, encoder := range knownEncoders() {
		if encoder.ID == id {
			return encoder, true
		}
	}
	return EncoderInfo{}, false
}

func knownEncoders() []EncoderInfo {
	return []EncoderInfo{
		// Software encoders.
		{ID: "libx264", Codec: "h264", Name: "x264 software", TwoPass: true},
		{ID: "libx265", Codec: "h265", Name: "x265 software", TwoPass: true},
		{ID: "libvvenc", Codec: "h266", Name: "vvenc H.266 software"},
		{ID: "libaom-av1", Codec: "av1", Name: "libaom AV1 software", TwoPass: true},
		{ID: "libvpx-vp9", Codec: "vp9", Name: "libvpx VP9 software", TwoPass: true},
		// No FFmpeg release ships an AV2 encoder yet; these IDs match the
		// expected names so the option lights up as soon as a build has one.
		{ID: "libavm", Codec: "av2", Name: "AVM AV2 software"},
		{ID: "libaom-av2", Codec: "av2", Name: "libaom AV2 software"},
		// NVIDIA NVENC.
		{ID: "h264_nvenc", Codec: "h264", Name: "NVIDIA NVENC H.264", Hardware: true},
		{ID: "hevc_nvenc", Codec: "h265", Name: "NVIDIA NVENC H.265", Hardware: true},
		{ID: "av1_nvenc", Codec: "av1", Name: "NVIDIA NVENC AV1", Hardware: true},
		// AMD AMF (proprietary driver stack).
		{ID: "h264_amf", Codec: "h264", Name: "AMD AMF H.264", Hardware: true},
		{ID: "hevc_amf", Codec: "h265", Name: "AMD AMF H.265", Hardware: true},
		{ID: "av1_amf", Codec: "av1", Name: "AMD AMF AV1", Hardware: true},
		// Intel Quick Sync.
		{ID: "h264_qsv", Codec: "h264", Name: "Intel QSV H.264", Hardware: true},
		{ID: "hevc_qsv", Codec: "h265", Name: "Intel QSV H.265", Hardware: true},
		{ID: "av1_qsv", Codec: "av1", Name: "Intel QSV AV1", Hardware: true},
		{ID: "vp9_qsv", Codec: "vp9", Name: "Intel QSV VP9", Hardware: true},
		// VAAPI (Mesa / Intel media driver).
		{ID: "h264_vaapi", Codec: "h264", Name: "VAAPI H.264 GPU", Hardware: true},
		{ID: "hevc_vaapi", Codec: "h265", Name: "VAAPI H.265 GPU", Hardware: true},
		{ID: "av1_vaapi", Codec: "av1", Name: "VAAPI AV1 GPU", Hardware: true},
		{ID: "vp9_vaapi", Codec: "vp9", Name: "VAAPI VP9 GPU", Hardware: true},
	}
}

func isVAAPIEncoder(id string) bool {
	return strings.HasSuffix(id, "_vaapi")
}

// detectWorkingHardware runs a tiny test encode for every hardware encoder the
// FFmpeg build advertises, because a build routinely lists NVENC, QSV, AMF, and
// VAAPI encoders that the local GPU and drivers cannot actually run. It returns
// the encoder IDs that produced frames, with the VAAPI device that worked.
func detectWorkingHardware(ffmpeg string, available map[string]bool) (map[string]bool, map[string]string) {
	working := make(map[string]bool)
	vaapiDevices := make(map[string]string)
	renderNodes, _ := filepath.Glob("/dev/dri/renderD*")

	type result struct {
		id     string
		device string
		ok     bool
	}
	var pending []EncoderInfo
	for _, encoder := range knownEncoders() {
		if encoder.Hardware && available[encoder.ID] {
			pending = append(pending, encoder)
		}
	}
	results := make(chan result, len(pending))
	var wait sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, encoder := range pending {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			if isVAAPIEncoder(id) {
				for _, node := range renderNodes {
					if runHardwareProbe(ffmpeg, id, node) {
						results <- result{id: id, device: node, ok: true}
						return
					}
				}
				results <- result{id: id}
				return
			}
			results <- result{id: id, ok: runHardwareProbe(ffmpeg, id, "")}
		}(encoder.ID)
	}
	wait.Wait()
	close(results)
	for item := range results {
		if item.ok {
			working[item.id] = true
			if item.device != "" {
				vaapiDevices[item.id] = item.device
			}
		}
	}
	return working, vaapiDevices
}

// runHardwareProbe encodes three 720p frames; small frames sit below some
// hardware minimum resolutions (AMD AV1 rejects anything under 256 lines).
func runHardwareProbe(ffmpeg, encoderID, vaapiDevice string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-v", "error", "-nostdin"}
	if vaapiDevice != "" {
		args = append(args, "-vaapi_device", vaapiDevice)
	}
	args = append(args, "-f", "lavfi", "-i", "color=black:size=1280x720:rate=30:duration=0.2")
	if vaapiDevice != "" {
		args = append(args, "-vf", "format=nv12,hwupload")
	}
	args = append(args, "-c:v", encoderID, "-b:v", "2M", "-frames:v", "3", "-f", "null", "-")
	return exec.CommandContext(ctx, ffmpeg, args...).Run() == nil
}

func inspectFFmpeg(ffmpeg string) (AppStatus, map[string]string, error) {
	versionOutput, err := exec.Command(ffmpeg, "-version").Output()
	if err != nil {
		return AppStatus{}, nil, errors.New("the bundled FFmpeg executable could not run")
	}
	firstLine := strings.SplitN(string(versionOutput), "\n", 2)[0]
	encoderOutput, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		return AppStatus{}, nil, errors.New("could not inspect the bundled FFmpeg encoders")
	}
	available := make(map[string]bool)
	for _, line := range strings.Split(string(encoderOutput), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && len(fields[0]) == 6 {
			available[fields[1]] = true
		}
	}
	workingHardware, vaapiDevices := detectWorkingHardware(ffmpeg, available)
	var encoders []EncoderInfo
	for _, encoder := range knownEncoders() {
		if !available[encoder.ID] {
			continue
		}
		if encoder.Hardware && !workingHardware[encoder.ID] {
			continue
		}
		encoders = append(encoders, encoder)
	}
	var audio []string
	for _, encoder := range []string{"aac", "libopus", "libvorbis", "libmp3lame"} {
		if available[encoder] {
			audio = append(audio, encoder)
		}
	}
	return AppStatus{FFmpegVersion: firstLine, Encoders: encoders, AudioEncoders: audio}, vaapiDevices, nil
}

func containerSupportsCodec(container, codec string) bool {
	supported := map[string]map[string]bool{
		"mp4":  {"h264": true, "h265": true, "h266": true, "av1": true},
		"mkv":  {"h264": true, "h265": true, "h266": true, "av1": true, "av2": true, "vp9": true},
		"webm": {"av1": true, "vp9": true},
		"mov":  {"h264": true, "h265": true, "av1": true},
	}
	return supported[container][codec]
}

func containerSupportsAudio(container, codec string) bool {
	if codec == "none" {
		return true
	}
	supported := map[string]map[string]bool{
		"mp4":  {"aac": true, "mp3": true},
		"mkv":  {"aac": true, "opus": true, "vorbis": true, "mp3": true},
		"webm": {"opus": true, "vorbis": true},
		"mov":  {"aac": true, "mp3": true},
	}
	return supported[container][codec]
}

func containerExtension(container string) string {
	if container == "mkv" || container == "webm" || container == "mov" {
		return container
	}
	return "mp4"
}

func muxerName(container string) string {
	switch container {
	case "mkv":
		return "matroska"
	case "webm":
		return "webm"
	case "mov":
		return "mov"
	default:
		return "mp4"
	}
}

func publishOutput(source, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open completed output: %w", err)
	}
	defer input.Close()
	temporary := destination + ".exactsize-copy"
	output, err := os.Create(temporary)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	if _, err := output.ReadFrom(input); err != nil {
		_ = output.Close()
		_ = os.Remove(temporary)
		return fmt.Errorf("copy output file: %w", err)
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publish output file: %w", err)
	}
	return nil
}

func removePassLogs(prefix string) {
	matches, _ := filepath.Glob(prefix + "*")
	for _, match := range matches {
		_ = os.Remove(match)
	}
}

func parseClock(value string) (float64, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0, false
	}
	hours, err1 := strconv.ParseFloat(parts[0], 64)
	minutes, err2 := strconv.ParseFloat(parts[1], 64)
	seconds, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return hours*3600 + minutes*60 + seconds, true
}

func progressSeconds(key, value string, fps float64) (float64, bool) {
	switch key {
	case "out_time_us", "out_time_ms":
		microseconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || microseconds < 0 {
			return 0, false
		}
		return float64(microseconds) / 1_000_000, true
	case "out_time":
		seconds, ok := parseClock(strings.TrimSpace(value))
		return seconds, ok && seconds >= 0
	case "frame":
		frame, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || frame < 0 || fps <= 0 {
			return 0, false
		}
		return frame / fps, true
	default:
		return 0, false
	}
}

func cleanFFmpegError(message string) string {
	message = strings.TrimSpace(message)
	lines := strings.Split(message, "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return strings.Join(lines, "\n")
}

type limitedBuffer struct {
	data bytes.Buffer
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	const limit = 64 << 10
	if b.data.Len()+len(data) > limit {
		excess := b.data.Len() + len(data) - limit
		current := b.data.Bytes()
		if excess >= len(current) {
			b.data.Reset()
		} else {
			copyData := append([]byte(nil), current[excess:]...)
			b.data.Reset()
			_, _ = b.data.Write(copyData)
		}
	}
	_, _ = b.data.Write(data)
	return len(data), nil
}

func (b *limitedBuffer) String() string { return b.data.String() }
