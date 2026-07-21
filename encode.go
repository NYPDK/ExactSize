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
	// Each distinct FPS or resolution setting gets this many opportunities
	// to converge on the target bitrate before the next fallback is tried.
	maximumEncodeAttempts = 8
	minimumOutputFPS      = 5
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
	Input            string  `json:"input"`
	Output           string  `json:"output"`
	TargetBytes      int64   `json:"targetBytes"`
	Container        string  `json:"container"`
	VideoCodec       string  `json:"videoCodec"`
	Encoder          string  `json:"encoder"`
	Preset           string  `json:"preset"`
	AudioCodec       string  `json:"audioCodec"`
	AudioBitrateKbps int     `json:"audioBitrateKbps"`
	AudioChannels    string  `json:"audioChannels"`
	TwoPass          bool    `json:"twoPass"`
	ResolutionHeight int     `json:"resolutionHeight"`
	AutoResolution   bool    `json:"autoResolution"`
	OutputFPS        float64 `json:"outputFps"`
	MinimumOutputFPS float64 `json:"minimumOutputFps"`
	Remux            bool    `json:"remux"`
	MuxAudio         bool    `json:"muxAudio"`
	VAAPIDevice      string  `json:"-"`
	ScaleWidth       int     `json:"-"`
	ScaleHeight      int     `json:"-"`
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

// allowedResolutionHeights are the selectable starting heights; zero means
// the source resolution. AutoResolution independently permits fallback.
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
	OutputFPS        float64 `json:"outputFps,omitempty"`
	Speed            string  `json:"speed"`
	Output           string  `json:"output"`
	Error            string  `json:"error,omitempty"`
}

type Job struct {
	request EncodeRequest
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
	// attemptStarted keeps retry ETA calculations independent from time spent
	// in earlier failed attempts.
	attemptStarted time.Time

	mu     sync.RWMutex
	status JobSnapshot
}

type outputSizeSample struct {
	seconds float64
	bytes   float64
}

type earlySizeCorrectionError struct {
	CurrentBytes    int64
	ProjectedBytes  int64
	EncodedSeconds  float64
	EncodedFraction float64
	ExceededTarget  bool
}

func (err *earlySizeCorrectionError) Error() string {
	if err.ExceededTarget {
		return "partial output exceeded the target before the encode completed"
	}
	return "partial output trajectory is projected to exceed the target"
}

type outputSizeMonitor struct {
	duration            float64
	targetBytes         int64
	sampleStartSeconds  float64
	decisionSeconds     float64
	minimumSampleSpan   float64
	maximumDecisionWall float64
	minimumMarginRatio  float64
	earlyMarginRatio    float64
	samples             []outputSizeSample
}

func newOutputSizeMonitor(duration float64, targetBytes int64, timeBounded bool) *outputSizeMonitor {
	if duration <= 0 || targetBytes <= 0 {
		return nil
	}
	monitor := &outputSizeMonitor{
		duration:           duration,
		targetBytes:        targetBytes,
		sampleStartSeconds: duration * 0.10,
		decisionSeconds:    duration * 0.25,
		minimumSampleSpan:  duration * 0.05,
		minimumMarginRatio: 0.02,
	}
	if timeBounded {
		// Hardware rate control and its minimum-bitrate floors become clear
		// quickly. Bound the checkpoint by encoded media time so a feature-length
		// source does not have to reach 25% before a futile attempt is stopped.
		monitor.sampleStartSeconds = min(monitor.sampleStartSeconds, 5)
		monitor.decisionSeconds = min(monitor.decisionSeconds, 30)
		monitor.minimumSampleSpan = min(monitor.minimumSampleSpan, 5)
		monitor.maximumDecisionWall = 60
		monitor.earlyMarginRatio = 0.30
	}
	return monitor
}

// observe records the growing output and decides whether the current attempt
// can still fit. Crossing the ceiling before completion is always terminal for
// the attempt. Software encodes use a 25% checkpoint. Hardware encodes cap the
// checkpoint at 30 seconds of encoded media or about one minute of wall time,
// so long and slow files do not spend many minutes proving a bitrate floor. A
// least-squares trajectory separates fixed mux/header bytes from ongoing
// stream growth.
func (monitor *outputSizeMonitor) observe(encodedSeconds float64, encodedBytes int64, wallSeconds float64) *earlySizeCorrectionError {
	if monitor == nil || encodedSeconds <= 0 || encodedBytes <= 0 {
		return nil
	}
	fraction := math.Max(0, math.Min(1, encodedSeconds/monitor.duration))
	if encodedBytes > monitor.targetBytes && fraction < 1 {
		return &earlySizeCorrectionError{
			CurrentBytes:    encodedBytes,
			ProjectedBytes:  encodedBytes,
			EncodedSeconds:  encodedSeconds,
			EncodedFraction: fraction,
			ExceededTarget:  true,
		}
	}
	if fraction >= 1 {
		return nil
	}
	if encodedSeconds < monitor.sampleStartSeconds {
		return nil
	}
	if count := len(monitor.samples); count > 0 {
		last := monitor.samples[count-1]
		if encodedSeconds <= last.seconds || float64(encodedBytes) < last.bytes {
			return nil
		}
	}
	monitor.samples = append(monitor.samples, outputSizeSample{seconds: encodedSeconds, bytes: float64(encodedBytes)})
	if len(monitor.samples) > 24 {
		monitor.samples = monitor.samples[len(monitor.samples)-24:]
	}
	checkpointReached := encodedSeconds >= monitor.decisionSeconds
	if monitor.maximumDecisionWall > 0 && wallSeconds >= monitor.maximumDecisionWall {
		checkpointReached = true
	}
	if !checkpointReached || len(monitor.samples) < 4 {
		return nil
	}
	first := monitor.samples[0]
	if encodedSeconds-first.seconds < monitor.minimumSampleSpan {
		return nil
	}
	projectedBytes := monitor.projectedBytes()
	confidenceMargin := max(int64(float64(monitor.targetBytes)*monitor.confidenceMarginRatio(encodedSeconds)), int64(256<<10))
	if projectedBytes <= monitor.targetBytes+confidenceMargin {
		return nil
	}
	return &earlySizeCorrectionError{
		CurrentBytes:    encodedBytes,
		ProjectedBytes:  projectedBytes,
		EncodedSeconds:  encodedSeconds,
		EncodedFraction: fraction,
	}
}

// confidenceMarginRatio reflects how representative the observed prefix can
// be. A 30-second opening may legitimately run well above the movie's average,
// so a time-bounded decision requires a clear miss. The allowance tightens as
// coverage grows and reaches the normal 2% margin at 25%.
func (monitor *outputSizeMonitor) confidenceMarginRatio(encodedSeconds float64) float64 {
	if monitor == nil {
		return 0
	}
	if monitor.earlyMarginRatio <= monitor.minimumMarginRatio || monitor.duration <= 0 {
		return monitor.minimumMarginRatio
	}
	fraction := math.Max(0, math.Min(1, encodedSeconds/monitor.duration))
	interpolate := func(value, from, to, start, end float64) float64 {
		if value <= from {
			return start
		}
		if value >= to {
			return end
		}
		return start + (end-start)*(value-from)/(to-from)
	}
	switch {
	case fraction <= 0.15:
		return monitor.earlyMarginRatio
	case fraction <= 0.20:
		return interpolate(fraction, 0.15, 0.20, monitor.earlyMarginRatio, 0.10)
	case fraction <= 0.25:
		return interpolate(fraction, 0.20, 0.25, 0.10, monitor.minimumMarginRatio)
	default:
		return monitor.minimumMarginRatio
	}
}

func (monitor *outputSizeMonitor) projectedBytes() int64 {
	if monitor == nil || len(monitor.samples) < 2 {
		return 0
	}
	var secondsMean, bytesMean float64
	for _, sample := range monitor.samples {
		secondsMean += sample.seconds
		bytesMean += sample.bytes
	}
	secondsMean /= float64(len(monitor.samples))
	bytesMean /= float64(len(monitor.samples))
	var covariance, variance float64
	for _, sample := range monitor.samples {
		secondsDelta := sample.seconds - secondsMean
		covariance += secondsDelta * (sample.bytes - bytesMean)
		variance += secondsDelta * secondsDelta
	}
	if variance <= 0 {
		return 0
	}
	slope := covariance / variance
	if slope <= 0 {
		return 0
	}
	intercept := bytesMean - slope*secondsMean
	projected := intercept + slope*monitor.duration
	lastBytes := monitor.samples[len(monitor.samples)-1].bytes
	if projected < lastBytes {
		projected = lastBytes
	}
	return int64(math.Ceil(projected))
}

func earlyCorrectionContext(correction *earlySizeCorrectionError, targetBytes int64) string {
	if correction == nil || targetBytes <= 0 {
		return ""
	}
	targetMB := float64(targetBytes) / 1_000_000
	if correction.ExceededTarget {
		return fmt.Sprintf("Stopped this attempt early because the partial output exceeded the %.1f MB target. ", targetMB)
	}
	percent := correction.EncodedFraction * 100
	formattedPercent := fmt.Sprintf("%.0f%%", percent)
	if percent < 10 {
		formattedPercent = fmt.Sprintf("%.1f%%", percent)
	}
	return fmt.Sprintf(
		"Stopped this attempt at %s because it was projected to finish at %.1f MB, above the %.1f MB target. ",
		formattedPercent,
		float64(correction.ProjectedBytes)/1_000_000,
		targetMB,
	)
}

func newJob(request EncodeRequest) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	return &Job{
		request:        request,
		ctx:            ctx,
		cancel:         cancel,
		started:        started,
		attemptStarted: started,
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
	// ResolutionHeight selects the starting size (zero means source). The
	// independent AutoResolution toggle controls whether correction may step
	// down from that starting point after bitrate and FPS are exhausted.
	startWidth, startHeight, autoResolution := startingResolution(j.request, info)
	j.request.ScaleWidth, j.request.ScaleHeight = startWidth, startHeight
	initialScaleWidth, initialScaleHeight := j.request.ScaleWidth, j.request.ScaleHeight
	useTwoPass := j.request.TwoPass && encoder.TwoPass
	passes := 1
	if useTwoPass {
		passes = 2
	}
	initialOutputFPS := effectiveOutputFPS(j.request, info)

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
		status.OutputFPS = initialOutputFPS
	})

	originalKbps := videoKbps
	previousRequestedKbps := 0
	previousActualKbps := 0
	previousScaleWidth := -1
	previousScaleHeight := -1
	previousOutputFPS := -1.0
	adaptiveFPSStage := 0
	maximumAttempts := correctionAttemptLimit(j.request, info, autoResolution)
	attemptMessage := ""

	for attempt := 1; attempt <= maximumAttempts; attempt++ {
		if err := j.ctx.Err(); err != nil {
			return err
		}
		if err := removePartialOutput(tempOutput); err != nil {
			return err
		}
		removePassLogs(passLog)
		progressFPS := effectiveOutputFPS(j.request, info)
		timeBoundedProjection := encoder.Hardware && needsTimeBoundedHardwareProjection(j.request, info, videoKbps)
		j.attemptStarted = time.Now()

		j.set(func(status *JobSnapshot) {
			status.Attempt = attempt
			status.VideoBitrateKbps = videoKbps
			status.EncodedBytes = 0
			status.Progress = 0
			status.Speed = ""
			status.Error = ""
			status.RemainingSeconds = 0
			status.OutputFPS = progressFPS
			status.Message = attemptMessage
		})

		var earlyCorrection *earlySizeCorrectionError
		if useTwoPass {
			firstPass := buildFFmpegArgs(j.request, info, videoKbps, tempOutput, passLog, 1, true)
			if err := j.runFFmpeg(ffmpeg, firstPass, info.Duration, progressFPS, 1, 2, attempt, tempOutput, 0, false); err != nil {
				return err
			}
			secondPass := buildFFmpegArgs(j.request, info, videoKbps, tempOutput, passLog, 2, true)
			if err := j.runFFmpeg(ffmpeg, secondPass, info.Duration, progressFPS, 2, 2, attempt, tempOutput, j.request.TargetBytes, timeBoundedProjection); err != nil {
				if !errors.As(err, &earlyCorrection) {
					return err
				}
			}
		} else {
			args := buildFFmpegArgs(j.request, info, videoKbps, tempOutput, passLog, 1, false)
			if err := j.runFFmpeg(ffmpeg, args, info.Duration, progressFPS, 1, 1, attempt, tempOutput, j.request.TargetBytes, timeBoundedProjection); err != nil {
				if !errors.As(err, &earlyCorrection) {
					return err
				}
			}
		}

		actualBytes := int64(0)
		earlyContext := ""
		if earlyCorrection != nil {
			actualBytes = max(earlyCorrection.ProjectedBytes, earlyCorrection.CurrentBytes)
			earlyContext = earlyCorrectionContext(earlyCorrection, j.request.TargetBytes)
			if err := removePartialOutput(tempOutput); err != nil {
				return err
			}
			j.set(func(status *JobSnapshot) {
				status.Phase = "Correcting"
				status.Message = earlyContext
				status.EncodedBytes = 0
			})
		} else {
			j.set(func(status *JobSnapshot) {
				status.Phase = "Verifying"
				status.Message = "Checking the exact output size…"
				status.Progress = 99
			})
			stat, err := os.Stat(tempOutput)
			if err != nil {
				return errors.New("FFmpeg finished without creating an output file")
			}
			actualBytes = stat.Size()
			j.set(func(status *JobSnapshot) { status.EncodedBytes = actualBytes })
		}

		if earlyCorrection == nil && actualBytes <= j.request.TargetBytes {
			if err := publishOutput(tempOutput, j.request.Output); err != nil {
				return err
			}
			finalOutputFPS := effectiveOutputFPS(j.request, info)
			adaptedFPS := finalOutputFPS < initialOutputFPS-0.001
			adaptedResolution := j.request.ScaleWidth != initialScaleWidth || j.request.ScaleHeight != initialScaleHeight
			details := make([]string, 0, 2)
			if j.request.ScaleHeight > 0 {
				details = append(details, fmt.Sprintf("%d×%d", j.request.ScaleWidth, j.request.ScaleHeight))
			}
			if adaptedFPS {
				details = append(details, formatFrameRate(finalOutputFPS)+" fps")
			}
			message := "Video compressed successfully"
			if len(details) > 0 {
				message += " at " + strings.Join(details, " and ")
			}
			if adaptedFPS || adaptedResolution {
				message += " (adapted to fit the target)"
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
		if earlyCorrection != nil {
			// A deliberately killed output has no finalized index/trailer to
			// probe. Its stable size trajectory still gives correction logic an
			// effective delivered bitrate without waiting for the doomed pass.
		} else if breakdown, probeErr := probeOutputBreakdown(j.ctx, ffprobe, tempOutput); probeErr != nil {
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

		if attempt == maximumAttempts {
			if encoder.Hardware {
				return fmt.Errorf("could not bring the output below the strict target after %d attempts; a software encoder, a lower resolution, or a different video codec may reach targets the GPU cannot", maximumAttempts)
			}
			return fmt.Errorf("could not bring the output below the strict target after %d attempts", maximumAttempts)
		}

		if encoder.Hardware {
			// Hardware encoders miss low targets additively: driver quality
			// floors (keyframes especially) add a roughly constant bitrate on
			// top of the request. Subtracting the measured excess converges
			// in one correction where a ratio cut needs several. When the
			// excess eats most of the budget, exhaust the minimum bitrate
			// before allowing FPS or Auto resolution to step down, unless a
			// prior same-setting correction already confirmed the floor.
			correctionBudgetKbps := originalKbps
			if availableVideoKbps > 0 {
				correctionBudgetKbps = hardwareSafeBitrate(availableVideoKbps)
			}
			confirmedFloor := previousScaleWidth == j.request.ScaleWidth &&
				previousScaleHeight == j.request.ScaleHeight &&
				math.Abs(previousOutputFPS-effectiveOutputFPS(j.request, info)) < 0.001 &&
				correctionHitBitrateFloor(previousRequestedKbps, previousActualKbps, videoKbps, actualVideoKbps, correctionBudgetKbps)
			previousRequestedKbps = videoKbps
			previousActualKbps = actualVideoKbps
			previousScaleWidth = j.request.ScaleWidth
			previousScaleHeight = j.request.ScaleHeight
			previousOutputFPS = effectiveOutputFPS(j.request, info)

			retryKbps, hopeless := hardwareCorrection(correctionBudgetKbps, videoKbps, actualVideoKbps)
			if likelyAV1VAAPIBitrateFloor(j.request, info, videoKbps, actualVideoKbps, correctionBudgetKbps) {
				hopeless = true
			}
			if confirmedFloor {
				hopeless = true
			}
			if hopeless {
				// Every FPS tier independently exhausts its bitrate options. A
				// confirmed floor counts as exhausted; otherwise probe the minimum
				// request before lowering FPS again or changing resolution.
				bitrateExhausted := bitrateOptionsExhausted(videoKbps, confirmedFloor)
				if !bitrateExhausted {
					minimumKbps, ok := minimumBitrateRetry(videoKbps)
					if !ok {
						return errors.New("could not exhaust the hardware encoder's bitrate options")
					}
					previousKbps := videoKbps
					videoKbps = minimumKbps
					attemptMessage = earlyContext + bitrateCorrectionMessage(previousKbps, videoKbps, effectiveOutputFPS(j.request, info))
					j.set(func(status *JobSnapshot) {
						status.Message = attemptMessage
					})
					continue
				}
				if nextFPS, ok := nextAdaptiveOutputFPS(j.request, info, actualVideoKbps, correctionBudgetKbps, adaptiveFPSStage > 0); ok {
					previousFPS := effectiveOutputFPS(j.request, info)
					j.request.OutputFPS = nextFPS
					adaptiveFPSStage++
					videoKbps = correctionBudgetKbps
					attemptMessage = earlyContext + fmt.Sprintf(
						"Bitrate options exhausted at %s fps; reducing frame rate to %s fps and restarting bitrate correction at %d kbps.",
						formatFrameRate(previousFPS),
						formatFrameRate(nextFPS),
						videoKbps,
					)
					j.set(func(status *JobSnapshot) {
						status.OutputFPS = nextFPS
						status.Message = attemptMessage
					})
					continue
				}
				if !autoResolution {
					return errors.New("the GPU encoder cannot reach this target at the selected resolution and frame-rate range; lower the minimum FPS or resolution, switch to a software encoder, or raise the target")
				}
				fpsOptionsExhausted := adaptiveFPSStage > 0
				adaptiveFPSStage = 3
				previousWidth, previousHeight := effectiveResolution(j.request, info)
				width, height, ok := floorAwareDownscale(info.Width, info.Height, j.request.ScaleHeight, actualVideoKbps, correctionBudgetKbps)
				if !ok {
					return errors.New("the GPU encoder cannot reach this target even at reduced resolution; switch to a software encoder, try a different video codec, or raise the target")
				}
				j.request.ScaleWidth, j.request.ScaleHeight = width, height
				videoKbps = correctionBudgetKbps
				if fpsOptionsExhausted {
					attemptMessage = earlyContext + fmt.Sprintf(
						"Bitrate options exhausted at %s fps; reducing resolution from %d×%d to %d×%d and restarting bitrate correction at %d kbps.",
						formatFrameRate(effectiveOutputFPS(j.request, info)),
						previousWidth,
						previousHeight,
						width,
						height,
						videoKbps,
					)
				} else {
					attemptMessage = earlyContext + fmt.Sprintf(
						"Bitrate options exhausted; reducing resolution from %d×%d to %d×%d and restarting bitrate correction at %d kbps.",
						previousWidth,
						previousHeight,
						width,
						height,
						videoKbps,
					)
				}
				j.set(func(status *JobSnapshot) {
					status.Message = attemptMessage
				})
				continue
			}
			if retryKbps > 0 {
				previousKbps := videoKbps
				videoKbps = retryKbps
				attemptMessage = earlyContext + bitrateCorrectionMessage(previousKbps, videoKbps, effectiveOutputFPS(j.request, info))
				j.set(func(status *JobSnapshot) {
					status.Message = attemptMessage
				})
				continue
			}
		}

		if availableVideoKbps > 0 && actualVideoKbps > 0 {
			correctionBudgetKbps := availableVideoKbps
			if encoder.Hardware {
				correctionBudgetKbps = hardwareSafeBitrate(correctionBudgetKbps)
			}
			previousKbps := videoKbps
			videoKbps = proportionalVideoCorrection(videoKbps, actualVideoKbps, correctionBudgetKbps)
			if videoKbps < minimumVideoBitrateKbps {
				return errors.New("the target is too small for this duration and measured non-video overhead")
			}
			attemptMessage = earlyContext + bitrateCorrectionMessage(previousKbps, videoKbps, effectiveOutputFPS(j.request, info))
			continue
		}

		// Scale by the measured overshoot and leave an extra 0.8%% margin.
		ratio := float64(j.request.TargetBytes) / float64(actualBytes)
		previousKbps := videoKbps
		videoKbps = int(math.Floor(float64(videoKbps) * ratio * 0.992))
		if videoKbps < minimumVideoBitrateKbps {
			return errors.New("the target is too small for this duration and audio bitrate")
		}
		attemptMessage = earlyContext + bitrateCorrectionMessage(previousKbps, videoKbps, effectiveOutputFPS(j.request, info))
	}

	return errors.New("compression ended unexpectedly")
}

func removePartialOutput(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("clean up incomplete output: %w", err)
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
	j.attemptStarted = time.Now()

	args := []string{
		"-hide_banner", "-y", "-nostdin", "-loglevel", "error", "-stats_period", "0.25",
		"-i", j.request.Input,
		"-map", "0:v:0", "-map", "0:a?",
		"-c:v", "copy",
	}
	if j.request.MuxAudio {
		args = append(args, audioEncoderArgs(j.request, info)...)
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

	if err := j.runFFmpeg(ffmpeg, args, info.Duration, info.FPS, 1, 1, 1, tempOutput, 0, false); err != nil {
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

func (j *Job) runFFmpeg(ffmpeg string, args []string, duration, fps float64, pass, passes, attempt int, output string, targetBytes int64, timeBoundedProjection bool) error {
	phase := "Encoding"
	if passes == 2 {
		phase = fmt.Sprintf("Encoding pass %d of 2", pass)
	}
	j.set(func(status *JobSnapshot) {
		status.State = "running"
		status.Phase = phase
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

	monitor := newOutputSizeMonitor(duration, targetBytes, timeBoundedProjection)
	monitorStarted := time.Now()
	var earlyCorrection *earlySizeCorrectionError
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), "=")
		if !found {
			continue
		}
		if seconds, ok := progressSeconds(key, value, fps); ok {
			encodedBytes := j.updateProgress(seconds, duration, pass, passes, attempt, output)
			if earlyCorrection == nil && monitor != nil {
				if decision := monitor.observe(seconds, encodedBytes, time.Since(monitorStarted).Seconds()); decision != nil {
					earlyCorrection = decision
					_ = command.Process.Kill()
				}
			}
		}
		switch key {
		case "speed":
			j.set(func(status *JobSnapshot) { status.Speed = strings.TrimSpace(value) })
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if earlyCorrection != nil {
		if j.ctx.Err() != nil {
			return j.ctx.Err()
		}
		return earlyCorrection
	}
	if scanErr != nil {
		if j.ctx.Err() != nil {
			return j.ctx.Err()
		}
		return scanErr
	}
	if waitErr != nil {
		if j.ctx.Err() != nil {
			return j.ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = waitErr.Error()
		}
		return fmt.Errorf("FFmpeg: %s", detail)
	}
	return nil
}

func (j *Job) updateProgress(encodedSeconds, duration float64, pass, passes, attempt int, output string) int64 {
	if duration <= 0 || encodedSeconds < 0 || math.IsNaN(encodedSeconds) || math.IsInf(encodedSeconds, 0) {
		return 0
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
	attemptElapsed := time.Since(j.attemptStarted).Seconds()
	remaining := float64(0)
	if overall > 0.5 {
		remaining = math.Max(0, attemptElapsed*(100-overall)/overall)
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
	return encodedBytes
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

	args = append(args, audioEncoderArgs(request, info)...)
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
		filters := make([]string, 0, 4)
		if filter := frameRateFilter(request, info); filter != "" {
			filters = append(filters, filter)
		}
		filters = append(filters, "format="+surface, "hwupload")
		if request.ScaleHeight > 0 {
			filters = append(filters, fmt.Sprintf("scale_vaapi=%d:%d", request.ScaleWidth, request.ScaleHeight))
		}
		args = append(args, "-vf", strings.Join(filters, ","))
	case request.Encoder == "libvvenc":
		// vvenc encodes Main 10 only.
		args = appendVideoFilters(args, request, info)
		args = append(args, "-pix_fmt", "yuv420p10le")
	case strings.HasSuffix(request.Encoder, "_nvenc"), strings.HasSuffix(request.Encoder, "_qsv"), strings.HasSuffix(request.Encoder, "_amf"):
		args = appendVideoFilters(args, request, info)
		if tenBitSource && request.VideoCodec != "h264" {
			args = append(args, "-pix_fmt", "p010le")
		} else {
			args = append(args, "-pix_fmt", "nv12")
		}
	case request.VideoCodec == "h264":
		args = appendVideoFilters(args, request, info)
		args = append(args, "-pix_fmt", "yuv420p")
	case tenBitSource:
		args = appendVideoFilters(args, request, info)
		args = append(args, "-pix_fmt", "yuv420p10le")
	default:
		args = appendVideoFilters(args, request, info)
		args = append(args, "-pix_fmt", "yuv420p")
	}
	return args
}

func appendVideoFilters(args []string, request EncodeRequest, info VideoInfo) []string {
	filters := make([]string, 0, 2)
	if filter := frameRateFilter(request, info); filter != "" {
		filters = append(filters, filter)
	}
	if request.ScaleHeight > 0 {
		filters = append(filters, fmt.Sprintf("scale=%d:%d:flags=lanczos", request.ScaleWidth, request.ScaleHeight))
	}
	if len(filters) == 0 {
		return args
	}
	return append(args, "-vf", strings.Join(filters, ","))
}

// effectiveOutputFPS returns the frame rate used for encoding and progress.
// A zero request preserves the source cadence, including variable frame rate.
func effectiveOutputFPS(request EncodeRequest, info VideoInfo) float64 {
	if request.OutputFPS >= minimumOutputFPS && (info.FPS <= 0 || request.OutputFPS < info.FPS) {
		return request.OutputFPS
	}
	return info.FPS
}

// adaptiveMinimumOutputFPS returns the lower end of a user-selected adaptive
// range. The slider's absolute-low position is intentionally a sentinel for a
// fixed frame rate, so 5 FPS (or zero from the UI) disables correction.
func adaptiveMinimumOutputFPS(request EncodeRequest, info VideoInfo) float64 {
	minimum := request.MinimumOutputFPS
	if minimum <= minimumOutputFPS || info.FPS <= 0 {
		return 0
	}
	if minimum >= effectiveOutputFPS(request, info)-0.001 {
		return 0
	}
	return minimum
}

// nextAdaptiveOutputFPS walks a selected range using at most three distinct
// rates: the already-tried maximum, a whole-number midpoint, then the minimum.
// It never crosses the selected minimum.
func nextAdaptiveOutputFPS(request EncodeRequest, info VideoInfo, actualKbps, budgetKbps int, exhaustRange bool) (float64, bool) {
	minimum := adaptiveMinimumOutputFPS(request, info)
	current := effectiveOutputFPS(request, info)
	if minimum == 0 || current <= minimum+0.001 || actualKbps <= budgetKbps || budgetKbps <= 0 {
		return 0, false
	}
	candidate := minimum
	if !exhaustRange {
		candidate = math.Round((current + minimum) / 2)
	}
	if candidate >= current-0.001 {
		candidate = math.Floor(current) - 1
	}
	if candidate < minimum {
		candidate = minimum
	}
	if candidate >= current-0.001 {
		return 0, false
	}
	return candidate, true
}

func formatFrameRate(fps float64) string {
	return strconv.FormatFloat(fps, 'f', -1, 64)
}

// correctionAttemptLimit gives every FPS tier and automatic-resolution rung
// the same bounded opportunity to converge on the target bitrate. Adaptive FPS
// has exactly three tiers: the selected maximum, midpoint, and minimum.
func correctionAttemptLimit(request EncodeRequest, info VideoInfo, autoResolution bool) int {
	settings := 1
	if adaptiveMinimumOutputFPS(request, info) > 0 {
		settings += 2
	}
	if autoResolution {
		settings += len(downscaleLadder)
	}
	return settings * maximumEncodeAttempts
}

func bitrateCorrectionMessage(previousKbps, nextKbps int, fps float64) string {
	return fmt.Sprintf(
		"Changing video bitrate from %d to %d kbps at %s fps before trying a lower frame rate or resolution.",
		previousKbps,
		nextKbps,
		formatFrameRate(fps),
	)
}

func frameRateFilter(request EncodeRequest, info VideoInfo) string {
	fps := effectiveOutputFPS(request, info)
	if request.OutputFPS < minimumOutputFPS || info.FPS <= 0 || fps >= info.FPS-0.001 {
		return ""
	}
	return "fps=" + strconv.FormatFloat(fps, 'f', 3, 64)
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

// startingResolution keeps the selected starting size independent from the
// automatic-fallback permission. A zero height means source dimensions, while
// AutoResolution may be enabled or disabled for any starting choice.
func startingResolution(request EncodeRequest, info VideoInfo) (width, height int, allowFallback bool) {
	allowFallback = request.AutoResolution
	if request.ResolutionHeight <= 0 {
		return 0, 0, allowFallback
	}
	width, height = scaleDimensions(info.Width, info.Height, request.ResolutionHeight)
	return width, height, allowFallback
}

func effectiveResolution(request EncodeRequest, info VideoInfo) (width, height int) {
	if request.ScaleWidth > 0 && request.ScaleHeight > 0 {
		return request.ScaleWidth, request.ScaleHeight
	}
	return info.Width, info.Height
}

// needsTimeBoundedHardwareProjection identifies settings where the requested
// rate is close enough to a GPU encoder floor that waiting for 25% would add
// little confidence. Ordinary higher-bitrate hardware jobs keep the more
// conservative percentage checkpoint so brief complex scenes cannot trigger
// needless quality reductions.
func needsTimeBoundedHardwareProjection(request EncodeRequest, info VideoInfo, videoKbps int) bool {
	if videoKbps <= 0 {
		return false
	}
	if videoKbps <= 256 {
		return true
	}
	width, height := effectiveResolution(request, info)
	fps := effectiveOutputFPS(request, info)
	if width <= 0 || height <= 0 || fps <= 0 {
		return false
	}
	bitsPerPixelFrame := float64(videoKbps*1000) / float64(width*height) / fps
	return bitsPerPixelFrame <= 0.012
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

// correctionHitBitrateFloor compares two completed attempts at the same
// resolution. A hardware encoder is at a confirmed floor when a meaningful
// request reduction yields less than half as much delivered-bitrate reduction
// and the newest stream still exceeds its budget. This direct comparison
// avoids spending another full pass on a bitrate the encoder cannot reach.
func correctionHitBitrateFloor(previousRequestedKbps, previousActualKbps, requestedKbps, actualKbps, budgetKbps int) bool {
	if previousRequestedKbps <= 0 || previousActualKbps <= 0 || requestedKbps <= 0 || actualKbps <= budgetKbps {
		return false
	}
	requestedDrop := previousRequestedKbps - requestedKbps
	minimumProbeDrop := previousRequestedKbps / 20
	if minimumProbeDrop < 4 {
		minimumProbeDrop = 4
	}
	if requestedDrop < minimumProbeDrop {
		return false
	}
	actualDrop := previousActualKbps - actualKbps
	minimumResponse := requestedDrop / 2
	if minimumResponse < 3 {
		minimumResponse = 3
	}
	return actualDrop < minimumResponse
}

// likelyAV1VAAPIBitrateFloor recognizes the severe low-bits-per-pixel miss
// seen when AV1 VAAPI reaches its quantizer floor. A normal correction cannot
// reclaim this much bitrate, so the correction path should try the minimum
// bitrate next, then use any adaptive FPS range before Auto resolution.
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
	fps := effectiveOutputFPS(request, info)
	if width <= 0 || height <= 0 || fps <= 0 {
		return false
	}
	bitsPerPixelFrame := float64(requestedKbps*1000) / float64(width*height) / fps
	return bitsPerPixelFrame <= 0.012
}

// minimumBitrateRetry exhausts the current resolution's bitrate option before
// a hardware encode is allowed to reduce FPS or resolution. Once the minimum
// has already failed, the caller can safely proceed to the adaptive fallbacks.
func minimumBitrateRetry(requestedKbps int) (int, bool) {
	if requestedKbps <= minimumVideoBitrateKbps {
		return 0, false
	}
	return minimumVideoBitrateKbps, true
}

// bitrateOptionsExhausted is the hard gate in front of every FPS and resolution
// fallback. Each distinct setting must complete a minimum-bitrate encode or
// produce direct evidence that lower requests no longer reduce the stream.
func bitrateOptionsExhausted(requestedKbps int, confirmedFloor bool) bool {
	return requestedKbps <= minimumVideoBitrateKbps || confirmedFloor
}

// hardwareCorrection turns an oversized hardware attempt into the next move.
// It returns a corrected bitrate request (measured excess subtracted with a
// 15% margin, since the excess grows slightly as requests shrink), or
// hopeless=true when the excess consumes over 60% of the budget and an
// ordinary correction is no longer useful. The caller then tries the minimum
// bitrate before considering lower FPS or resolution unless attempt history
// already confirmed the floor. Both zero values mean the overshoot was not
// additive; the caller falls back to proportional correction.
func hardwareCorrection(budgetKbps, requestedKbps, actualKbps int) (int, bool) {
	excess := actualKbps - requestedKbps
	if excess <= 0 {
		return 0, false
	}
	corrected := budgetKbps - excess*23/20
	if corrected < minimumVideoBitrateKbps || corrected*10 < budgetKbps*4 {
		return 0, true
	}
	// Do not spend another long attempt testing a value only a few kbps above
	// the encoder minimum. Probe the minimum directly; if it misses too, the
	// next correction can move to the selected FPS or resolution fallback.
	nearMinimum := max(8, budgetKbps/10)
	if corrected <= minimumVideoBitrateKbps+nearMinimum {
		return minimumVideoBitrateKbps, false
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

func audioEncoderArgs(request EncodeRequest, info VideoInfo) []string {
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
	case "source":
		if request.AudioCodec == "opus" {
			streams := info.audioStreams
			if len(streams) == 0 && info.AudioChannels > 0 {
				streams = []audioStreamInfo{{Channels: info.AudioChannels, ChannelLayout: info.AudioChannelLayout}}
			}
			for index, stream := range streams {
				if stream.Channels <= 2 {
					continue
				}
				layout, mappingFamily := opusLayoutMapping(stream.Channels, stream.ChannelLayout)
				streamSpecifier := strconv.Itoa(index)
				if layout != "" && layout != stream.ChannelLayout {
					args = append(args, "-filter:a:"+streamSpecifier, "aformat=channel_layouts="+layout)
				}
				args = append(args, "-mapping_family:a:"+streamSpecifier, strconv.Itoa(mappingFamily))
			}
		}
	}
	return args
}

// opusLayoutMapping selects an explicit mapping for multichannel Opus. The
// libopus default (-1) rejects common Blu-ray layouts such as 5.1(side).
// Opus mapping family 1 defines interoperable surround layouts; side variants
// are normalized to their equivalent family-1 layout. Less common layouts use
// family 255 so keeping the source channel count still encodes successfully.
func opusLayoutMapping(channels int, channelLayout string) (layout string, mappingFamily int) {
	switch channelLayout {
	case "3.0", "quad", "5.0", "5.1", "6.1", "7.1":
		return channelLayout, 1
	case "quad(side)":
		return "quad", 1
	case "5.0(side)":
		return "5.0", 1
	case "5.1(side)":
		return "5.1", 1
	case "":
		canonical := map[int]string{3: "3.0", 4: "quad", 5: "5.0", 6: "5.1", 7: "6.1", 8: "7.1"}
		if layout := canonical[channels]; layout != "" {
			return layout, 1
		}
	}
	return "", 255
}

func calculateVideoBitrate(request EncodeRequest, info VideoInfo) (int, error) {
	if request.TargetBytes <= 0 || info.Duration <= 0 {
		return 0, errors.New("target size and duration must be greater than zero")
	}
	outputInfo := info
	outputInfo.FPS = effectiveOutputFPS(request, info)
	reserve := estimatedMuxOverheadBytes(request.Container, outputInfo, request.AudioCodec)
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
	if request.OutputFPS != 0 && !validRequestedFPS(request.OutputFPS) {
		return fmt.Errorf("maximum output frame rate must be a whole number of at least %d fps", minimumOutputFPS)
	}
	if request.MinimumOutputFPS != 0 && !validRequestedFPS(request.MinimumOutputFPS) {
		return fmt.Errorf("minimum output frame rate must be a whole number of at least %d fps", minimumOutputFPS)
	}
	if request.MinimumOutputFPS > minimumOutputFPS && request.OutputFPS > 0 && request.MinimumOutputFPS > request.OutputFPS {
		return errors.New("minimum output frame rate cannot exceed the maximum")
	}
	return nil
}

func validRequestedFPS(fps float64) bool {
	return !math.IsNaN(fps) && !math.IsInf(fps, 0) && fps >= minimumOutputFPS && math.Abs(fps-math.Round(fps)) < 0.001
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
	if !j.request.Remux && (j.request.OutputFPS > 0 || j.request.MinimumOutputFPS > minimumOutputFPS) {
		if info.FPS <= 0 {
			return errors.New("the input frame rate could not be determined")
		}
		if j.request.OutputFPS > info.FPS+0.001 {
			return errors.New("the maximum output frame rate cannot exceed the input frame rate")
		}
		if j.request.MinimumOutputFPS > info.FPS+0.001 {
			return errors.New("the minimum output frame rate cannot exceed the input frame rate")
		}
		if j.request.MinimumOutputFPS > minimumOutputFPS && j.request.MinimumOutputFPS > effectiveOutputFPS(j.request, info)+0.001 {
			return errors.New("the minimum output frame rate cannot exceed the maximum")
		}
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
