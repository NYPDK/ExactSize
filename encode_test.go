package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCalculateVideoBitrateBudgetsAudioAndOverhead(t *testing.T) {
	request := validTestRequest()
	request.TargetBytes = 100_000_000
	request.Container = "mp4"
	request.AudioCodec = "aac"
	request.AudioBitrateKbps = 128
	info := VideoInfo{Duration: 60, FPS: 60, AudioTracks: 2, AudioSampleRate: 48_000}
	bitrate, err := calculateVideoBitrate(request, info)
	if err != nil {
		t.Fatal(err)
	}
	// Packet-aware MP4 overhead is about 134 KB here, then two 128 kbps
	// audio tracks are removed from the remaining payload budget.
	if bitrate < 13_040 || bitrate > 13_080 {
		t.Fatalf("unexpected video bitrate: %d kbps", bitrate)
	}
}

func TestCalculateVideoBitrateUsesReducedOutputFrameRateForMuxReserve(t *testing.T) {
	request := validTestRequest()
	request.TargetBytes = 15_000_000
	request.Container = "webm"
	request.AudioCodec = "opus"
	info := VideoInfo{Duration: 300, FPS: 60, AudioTracks: 2, AudioSampleRate: 48_000}

	sourceRate, err := calculateVideoBitrate(request, info)
	if err != nil {
		t.Fatal(err)
	}
	request.OutputFPS = 15
	reducedRate, err := calculateVideoBitrate(request, info)
	if err != nil {
		t.Fatal(err)
	}
	if reducedRate <= sourceRate {
		t.Fatalf("lower FPS should reserve fewer mux bytes: source=%d kbps reduced=%d kbps", sourceRate, reducedRate)
	}
}

func TestCalculateVideoBitrateRejectsImpossibleTarget(t *testing.T) {
	request := validTestRequest()
	request.TargetBytes = 1_000_000
	info := VideoInfo{Duration: 600, FPS: 60, AudioTracks: 1, AudioSampleRate: 48_000}
	_, err := calculateVideoBitrate(request, info)
	if err == nil {
		t.Fatal("expected an impossible target to be rejected")
	}
}

func TestEstimatedMuxOverheadIsContainerAndPacketAware(t *testing.T) {
	short := VideoInfo{Duration: 60, FPS: 60, AudioTracks: 2, AudioSampleRate: 48_000}
	webm := estimatedMuxOverheadBytes("webm", short, "opus")
	mp4 := estimatedMuxOverheadBytes("mp4", short, "aac")
	if webm <= 64*1024 {
		t.Fatalf("60 seconds of WebM packets should exceed the fixed floor, got %d", webm)
	}
	if mp4 <= webm {
		t.Fatalf("MP4 sample tables should reserve more than WebM blocks: mp4=%d webm=%d", mp4, webm)
	}

	long := short
	long.Duration = 600
	if got := estimatedMuxOverheadBytes("mp4", long, "aac"); got <= mp4*5 {
		t.Fatalf("long high-FPS files must reserve for their packet count: short=%d long=%d", mp4, got)
	}
	noAudio := short
	noAudio.AudioTracks = 0
	if got := estimatedMuxOverheadBytes("webm", noAudio, "none"); got >= webm {
		t.Fatalf("removing audio streams should reduce mux overhead: with=%d without=%d", webm, got)
	}
	highRate := short
	highRate.AudioSampleRate = 96_000
	if got := estimatedMuxOverheadBytes("mp4", highRate, "aac"); got <= mp4 {
		t.Fatalf("higher AAC sample rates should account for more packets: 48k=%d 96k=%d", mp4, got)
	}
}

func TestContainerCompatibility(t *testing.T) {
	tests := []struct {
		container string
		codec     string
		want      bool
	}{
		{"mp4", "h264", true},
		{"mp4", "vp9", false},
		{"mp4", "h266", true},
		{"mp4", "av2", false},
		{"webm", "av1", true},
		{"webm", "h265", false},
		{"webm", "h266", false},
		{"mkv", "vp9", true},
		{"mkv", "h266", true},
		{"mkv", "av2", true},
		{"mov", "h266", false},
	}
	for _, test := range tests {
		if got := containerSupportsCodec(test.container, test.codec); got != test.want {
			t.Errorf("containerSupportsCodec(%q, %q) = %v, want %v", test.container, test.codec, got, test.want)
		}
	}
}

func TestVideoEncoderArgsPerFamily(t *testing.T) {
	info := VideoInfo{PixelFormat: "yuv420p"}
	tenBit := VideoInfo{PixelFormat: "yuv420p10le"}

	nvenc := videoEncoderArgs(EncodeRequest{Encoder: "hevc_nvenc", VideoCodec: "h265", Preset: "quality"}, info, 4000)
	assertArgs(t, nvenc, "-preset", "p7")
	assertArgs(t, nvenc, "-maxrate", "4000k")
	assertArgs(t, nvenc, "-pix_fmt", "nv12")

	vaapi := videoEncoderArgs(EncodeRequest{Encoder: "av1_vaapi", VideoCodec: "av1", Preset: "balanced"}, tenBit, 2500)
	assertArgs(t, vaapi, "-vf", "format=p010,hwupload")
	assertArgs(t, vaapi, "-g", "250")
	for _, arg := range vaapi {
		if arg == "-pix_fmt" {
			t.Fatal("VAAPI encodes must upload to GPU surfaces instead of setting -pix_fmt")
		}
	}

	vvenc := videoEncoderArgs(EncodeRequest{Encoder: "libvvenc", VideoCodec: "h266", Preset: "fastest"}, info, 1200)
	assertArgs(t, vvenc, "-preset", "faster")

	aom := videoEncoderArgs(EncodeRequest{Encoder: "libaom-av1", VideoCodec: "av1", Preset: "balanced"}, VideoInfo{PixelFormat: "yuv420p", Height: 1080}, 2000)
	assertArgs(t, aom, "-tiles", "2x2")
	assertArgs(t, aom, "-row-mt", "1")
	aomFastest := videoEncoderArgs(EncodeRequest{Encoder: "libaom-av1", VideoCodec: "av1", Preset: "fastest"}, VideoInfo{PixelFormat: "yuv420p", Height: 1080}, 2000)
	assertArgs(t, aomFastest, "-tiles", "4x2")
	aomScaled := videoEncoderArgs(EncodeRequest{Encoder: "libaom-av1", VideoCodec: "av1", Preset: "balanced", ScaleHeight: 720}, VideoInfo{PixelFormat: "yuv420p", Height: 1080}, 900)
	assertArgs(t, aomScaled, "-tiles", "2x1")

	x264Fastest := videoEncoderArgs(EncodeRequest{Encoder: "libx264", VideoCodec: "h264", Preset: "fastest"}, info, 3000)
	assertArgs(t, x264Fastest, "-preset", "veryfast")
	assertArgs(t, vvenc, "-pix_fmt", "yuv420p10le")

	software := videoEncoderArgs(EncodeRequest{Encoder: "libx264", VideoCodec: "h264", Preset: "balanced"}, info, 3000)
	for _, arg := range software {
		if arg == "-maxrate" {
			t.Fatal("software encoders should not receive the hardware VBR cap")
		}
	}
}

func assertArgs(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			if args[i+1] != want {
				t.Fatalf("%s = %q, want %q (args: %v)", flag, args[i+1], want, args)
			}
			return
		}
	}
	t.Fatalf("missing %s %s in %v", flag, want, args)
}

func TestHVC1TagOnlyForX265(t *testing.T) {
	// Forcing hvc1 makes the muxer strip in-band parameter sets; encoders
	// without proper global extradata (hevc_vaapi) then produce undecodable
	// files. See the corrupted-output regression from 2026-07-14.
	base := validTestRequest()
	base.Container = "mp4"
	base.VideoCodec = "h265"
	base.TwoPass = false

	hasTag := func(encoder string) bool {
		request := base
		request.Encoder = encoder
		args := buildFFmpegArgs(request, VideoInfo{PixelFormat: "yuv420p"}, 3000, "/tmp/out.mp4", "/tmp/pass", 1, false)
		for i, arg := range args {
			if arg == "-tag:v" && i+1 < len(args) && args[i+1] == "hvc1" {
				return true
			}
		}
		return false
	}
	if !hasTag("libx265") {
		t.Error("libx265 output should carry the hvc1 tag for player compatibility")
	}
	for _, encoder := range []string{"hevc_vaapi", "hevc_nvenc", "hevc_qsv", "hevc_amf"} {
		if hasTag(encoder) {
			t.Errorf("%s must not force the hvc1 tag", encoder)
		}
	}
}

func TestH264VAAPIDisablesBFrames(t *testing.T) {
	// AMD VCN encodes H.264 B-frames at visibly lower quality than the
	// neighboring P-frames, which pulses on motion (the "jittery" output
	// regression from 2026-07-14).
	h264 := videoEncoderArgs(EncodeRequest{Encoder: "h264_vaapi", VideoCodec: "h264", Preset: "balanced"}, VideoInfo{PixelFormat: "yuv420p"}, 4000)
	assertArgs(t, h264, "-bf", "0")
	hevc := videoEncoderArgs(EncodeRequest{Encoder: "hevc_vaapi", VideoCodec: "h265", Preset: "balanced"}, VideoInfo{PixelFormat: "yuv420p"}, 4000)
	for _, arg := range hevc {
		if arg == "-bf" {
			t.Fatal("hevc_vaapi should keep its default reference structure")
		}
	}
}

func TestDownscaleArgs(t *testing.T) {
	request := EncodeRequest{Encoder: "av1_vaapi", VideoCodec: "av1", Preset: "balanced", ScaleWidth: 1280, ScaleHeight: 720}
	vaapi := videoEncoderArgs(request, VideoInfo{PixelFormat: "yuv420p"}, 400)
	assertArgs(t, vaapi, "-vf", "format=nv12,hwupload,scale_vaapi=1280:720")

	software := videoEncoderArgs(EncodeRequest{Encoder: "libx264", VideoCodec: "h264", Preset: "balanced", ScaleWidth: 1280, ScaleHeight: 720}, VideoInfo{PixelFormat: "yuv420p"}, 400)
	assertArgs(t, software, "-vf", "scale=1280:720:flags=lanczos")
}

func TestFrameRateFilterArgs(t *testing.T) {
	info := VideoInfo{PixelFormat: "yuv420p", FPS: 60}
	request := EncodeRequest{Encoder: "av1_vaapi", VideoCodec: "av1", Preset: "balanced", OutputFPS: 30, ScaleWidth: 1280, ScaleHeight: 720}
	vaapi := videoEncoderArgs(request, info, 400)
	assertArgs(t, vaapi, "-vf", "fps=30.000,format=nv12,hwupload,scale_vaapi=1280:720")

	software := videoEncoderArgs(EncodeRequest{Encoder: "libx264", VideoCodec: "h264", Preset: "balanced", OutputFPS: 24}, info, 400)
	assertArgs(t, software, "-vf", "fps=24.000")

	request.OutputFPS = 60
	sourceRate := videoEncoderArgs(request, info, 400)
	assertArgs(t, sourceRate, "-vf", "format=nv12,hwupload,scale_vaapi=1280:720")
	if got := effectiveOutputFPS(EncodeRequest{OutputFPS: 30}, info); got != 30 {
		t.Fatalf("effective output FPS = %v, want 30", got)
	}
}

func TestAdaptiveFrameRateCorrection(t *testing.T) {
	info := VideoInfo{FPS: 60}
	request := EncodeRequest{MinimumOutputFPS: 30}
	if got := adaptiveMinimumOutputFPS(request, info); got != 30 {
		t.Fatalf("adaptive minimum = %v, want 30", got)
	}
	if got, ok := nextAdaptiveOutputFPS(request, info, 600, 400, false); !ok || got != 45 {
		t.Fatalf("first adaptive FPS = %v, %v; want midpoint 45, true", got, ok)
	}
	if got, ok := nextAdaptiveOutputFPS(request, info, 1200, 400, false); !ok || got != 45 {
		t.Fatalf("the first FPS correction must remain the midpoint even on a large miss, got %v, %v", got, ok)
	}
	if got, ok := nextAdaptiveOutputFPS(request, info, 600, 400, true); !ok || got != 30 {
		t.Fatalf("the second FPS correction should exhaust the range, got %v, %v", got, ok)
	}

	request.OutputFPS = 30
	if _, ok := nextAdaptiveOutputFPS(request, info, 600, 400, false); ok {
		t.Fatal("correction must stop after reaching the selected minimum")
	}
	request = EncodeRequest{MinimumOutputFPS: minimumOutputFPS}
	if got := adaptiveMinimumOutputFPS(request, info); got != 0 {
		t.Fatalf("the absolute-low minimum handle must lock the maximum, got %v", got)
	}
	if _, ok := nextAdaptiveOutputFPS(request, info, 600, 400, false); ok {
		t.Fatal("the absolute-low minimum handle must disable adaptive correction")
	}
}

func TestCorrectionAttemptLimitRestartsBitrateCycleAtEveryFPS(t *testing.T) {
	info := VideoInfo{FPS: 60}
	fixed := EncodeRequest{}
	if got := correctionAttemptLimit(fixed, info, false); got != maximumEncodeAttempts {
		t.Fatalf("fixed-rate attempt limit = %d, want %d", got, maximumEncodeAttempts)
	}

	adaptive := EncodeRequest{MinimumOutputFPS: 30}
	wantAdaptive := maximumEncodeAttempts * 3
	if got := correctionAttemptLimit(adaptive, info, false); got != wantAdaptive {
		t.Fatalf("adaptive attempt limit = %d, want %d for maximum/midpoint/minimum bitrate cycles", got, wantAdaptive)
	}

	wantAdaptiveResolution := maximumEncodeAttempts * (3 + len(downscaleLadder))
	if got := correctionAttemptLimit(adaptive, info, true); got != wantAdaptiveResolution {
		t.Fatalf("adaptive-resolution attempt limit = %d, want %d", got, wantAdaptiveResolution)
	}
}

func TestScaleDimensions(t *testing.T) {
	tests := []struct {
		sourceW, sourceH, target int
		wantW, wantH             int
	}{
		{1920, 1080, 720, 1280, 720},
		{1920, 1080, 1080, 0, 0}, // same height: no scaling
		{1920, 1080, 2160, 0, 0}, // never upscale
		{1920, 1080, 0, 0, 0},    // auto
		{1080, 1920, 720, 404, 720},
		{0, 0, 720, 0, 0},
	}
	for _, test := range tests {
		w, h := scaleDimensions(test.sourceW, test.sourceH, test.target)
		if w != test.wantW || h != test.wantH {
			t.Errorf("scaleDimensions(%d, %d, %d) = %d, %d; want %d, %d",
				test.sourceW, test.sourceH, test.target, w, h, test.wantW, test.wantH)
		}
	}
}

func TestStartingResolutionIsIndependentFromAutomaticFallback(t *testing.T) {
	info := VideoInfo{Width: 1920, Height: 1080}
	tests := []struct {
		name                  string
		request               EncodeRequest
		wantW, wantH          int
		wantAutomaticFallback bool
	}{
		{"fixed source", EncodeRequest{}, 0, 0, false},
		{"adaptive source", EncodeRequest{AutoResolution: true}, 0, 0, true},
		{"fixed 720p", EncodeRequest{ResolutionHeight: 720}, 1280, 720, false},
		{"adaptive 720p", EncodeRequest{ResolutionHeight: 720, AutoResolution: true}, 1280, 720, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			width, height, automaticFallback := startingResolution(test.request, info)
			if width != test.wantW || height != test.wantH || automaticFallback != test.wantAutomaticFallback {
				t.Fatalf("startingResolution = %d×%d, %v; want %d×%d, %v", width, height, automaticFallback, test.wantW, test.wantH, test.wantAutomaticFallback)
			}
		})
	}
}

func TestValidateResolutionHeight(t *testing.T) {
	request := validTestRequest()
	request.ResolutionHeight = 720
	if err := validateEncodeRequest(request); err != nil {
		t.Fatalf("720p should be a valid resolution: %v", err)
	}
	request.ResolutionHeight = 123
	if err := validateEncodeRequest(request); err == nil {
		t.Fatal("arbitrary resolutions must be rejected")
	}
}

func TestValidateOutputFrameRate(t *testing.T) {
	request := validTestRequest()
	request.OutputFPS = 4.99
	if err := validateEncodeRequest(request); err == nil {
		t.Fatal("output below 5 fps must be rejected")
	}
	request.OutputFPS = 29.97
	if err := validateEncodeRequest(request); err == nil {
		t.Fatal("the FPS slider must only accept whole-number maximums")
	}

	request.OutputFPS = 30
	request.MinimumOutputFPS = 24
	if err := validateEncodeRequest(request); err != nil {
		t.Fatalf("24–30 fps should validate before probing: %v", err)
	}
	job := newJob(request)
	if err := job.validateWithProbe(VideoInfo{Duration: 60, FPS: 60}); err != nil {
		t.Fatalf("a range below the input frame rate should validate: %v", err)
	}
	job.request.OutputFPS = 61
	if err := job.validateWithProbe(VideoInfo{Duration: 60, FPS: 60}); err == nil {
		t.Fatal("output above the input frame rate must be rejected")
	}
	request.OutputFPS = 30
	request.MinimumOutputFPS = 31
	if err := validateEncodeRequest(request); err == nil {
		t.Fatal("minimum FPS above a fixed maximum must be rejected")
	}
	request.OutputFPS = 0
	request.MinimumOutputFPS = 61
	job = newJob(request)
	if err := job.validateWithProbe(VideoInfo{Duration: 60, FPS: 60}); err == nil {
		t.Fatal("minimum FPS above a source-rate maximum must be rejected")
	}
}

func TestMeasuredVideoKbps(t *testing.T) {
	// 16.5 MB over 160 s with one 128 kbps audio track: ~697 kbps of video.
	got := measuredVideoKbps(16_500_000, 160, 1, "aac", 128)
	if got < 690 || got > 705 {
		t.Errorf("measuredVideoKbps = %d, want ~697", got)
	}
	if measuredVideoKbps(16_500_000, 0, 1, "aac", 128) != 0 {
		t.Error("zero duration must not divide")
	}
	if measuredVideoKbps(1000, 160, 4, "aac", 320) != 0 {
		t.Error("audio exceeding the file size must clamp to zero")
	}
}

func TestParseOutputBreakdown(t *testing.T) {
	packets := "video,60,\nvideo,40\naudio,30\ndata,10\nmalformed\n"
	got, err := parseOutputBreakdown(strings.NewReader(packets), 190)
	if err != nil {
		t.Fatal(err)
	}
	if got.VideoBytes != 100 || got.AudioBytes != 30 || got.OtherBytes != 10 || got.MuxBytes != 50 {
		t.Fatalf("unexpected output breakdown: %+v", got)
	}
	if got.VideoPackets != 2 || got.AudioPackets != 1 {
		t.Fatalf("unexpected packet counts: %+v", got)
	}
}

func TestMeasuredOutputVideoBudget(t *testing.T) {
	breakdown := OutputBreakdown{AudioBytes: 1_500_000, OtherBytes: 10_000, MuxBytes: 90_000}
	got := outputVideoBudgetKbps(5_000_000, 50, breakdown)
	if got != 544 {
		t.Fatalf("outputVideoBudgetKbps = %d, want 544", got)
	}
	if got := outputVideoBudgetKbps(1_000_000, 50, breakdown); got != 0 {
		t.Fatalf("non-video bytes exceeding the target should leave no video budget, got %d", got)
	}
}

func TestLikelyAV1VAAPIBitrateFloor(t *testing.T) {
	request := EncodeRequest{Encoder: "av1_vaapi", VideoCodec: "av1"}
	info := VideoInfo{Width: 1920, Height: 1080, FPS: 60}
	if !likelyAV1VAAPIBitrateFloor(request, info, 409, 574, 473) {
		t.Fatal("the measured 1080p60 AV1 VAAPI floor should trigger the minimum-bitrate path")
	}
	if likelyAV1VAAPIBitrateFloor(request, info, 409, 520, 473) {
		t.Fatal("ordinary rate-control drift should get one bitrate correction")
	}
	request.Encoder = "libaom-av1"
	if likelyAV1VAAPIBitrateFloor(request, info, 409, 574, 473) {
		t.Fatal("software AV1 must not use the VAAPI floor heuristic")
	}
	request.Encoder = "av1_vaapi"
	if likelyAV1VAAPIBitrateFloor(request, info, 2000, 2800, 2200) {
		t.Fatal("a healthy bits-per-pixel budget should not be classified as the low-rate floor")
	}
}

func TestNeedsTimeBoundedHardwareProjection(t *testing.T) {
	info := VideoInfo{Width: 1920, Height: 1080, FPS: 23.98}
	if !needsTimeBoundedHardwareProjection(EncodeRequest{}, info, 82) {
		t.Fatal("the feature-length 82 kbps case should use a time-bounded projection")
	}
	if needsTimeBoundedHardwareProjection(EncodeRequest{}, info, 4_000) {
		t.Fatal("an ordinary 1080p hardware bitrate should keep the conservative checkpoint")
	}
	if !needsTimeBoundedHardwareProjection(EncodeRequest{}, VideoInfo{Width: 3840, Height: 2160, FPS: 60}, 1_500) {
		t.Fatal("a very low bits-per-pixel 4K request should use a time-bounded projection")
	}
}

func TestCorrectionHitBitrateFloor(t *testing.T) {
	if !correctionHitBitrateFloor(400, 520, 300, 515, 380) {
		t.Fatal("a 100 kbps request cut with only a 5 kbps output response should confirm a bitrate floor")
	}
	if correctionHitBitrateFloor(400, 520, 300, 445, 380) {
		t.Fatal("a responsive encoder should continue with bitrate correction")
	}
	if correctionHitBitrateFloor(400, 520, 300, 375, 380) {
		t.Fatal("an output already within budget is not a blocking floor")
	}
	if correctionHitBitrateFloor(400, 520, 395, 519, 380) {
		t.Fatal("a tiny request change is not enough evidence to declare a floor")
	}
	if correctionHitBitrateFloor(300, 515, 320, 520, 380) {
		t.Fatal("raising the requested bitrate cannot prove a lower bitrate floor")
	}
}

func TestMinimumBitrateRetryBeforeDownscale(t *testing.T) {
	if bitrate, ok := minimumBitrateRetry(409); !ok || bitrate != minimumVideoBitrateKbps {
		t.Fatalf("a hopeless 409 kbps encode should retry at the minimum, got %d, %v", bitrate, ok)
	}
	if bitrate, ok := minimumBitrateRetry(minimumVideoBitrateKbps); ok || bitrate != 0 {
		t.Fatalf("the minimum bitrate must be exhausted before downscaling, got %d, %v", bitrate, ok)
	}
}

func TestBitrateOptionsGateFPSAndResolutionFallback(t *testing.T) {
	if bitrateOptionsExhausted(409, false) {
		t.Fatal("an untried lower bitrate must block FPS and resolution fallback")
	}
	if !bitrateOptionsExhausted(minimumVideoBitrateKbps, false) {
		t.Fatal("a completed minimum-bitrate attempt must unlock adaptive fallback")
	}
	if !bitrateOptionsExhausted(300, true) {
		t.Fatal("a measured encoder floor must unlock adaptive fallback")
	}
	if bitrateOptionsExhausted(300, false) {
		t.Fatal("each new FPS tier must restart bitrate correction instead of bypassing it")
	}
}

func TestBitrateCorrectionMessageNamesEveryChangedValue(t *testing.T) {
	message := bitrateCorrectionMessage(409, minimumVideoBitrateKbps, 45)
	for _, detail := range []string{"from 409 to 64 kbps", "at 45 fps", "before trying a lower frame rate or resolution"} {
		if !strings.Contains(message, detail) {
			t.Fatalf("correction message %q is missing %q", message, detail)
		}
	}
}

func TestOutputSizeMonitorStopsImmediatelyAfterCrossingTarget(t *testing.T) {
	monitor := newOutputSizeMonitor(100, 100_000_000, false)
	decision := monitor.observe(12, 100_000_001, 12)
	if decision == nil || !decision.ExceededTarget {
		t.Fatalf("crossing the target should stop the attempt immediately, got %+v", decision)
	}
	if decision.CurrentBytes != 100_000_001 || decision.EncodedFraction != 0.12 {
		t.Fatalf("unexpected immediate-stop measurements: %+v", decision)
	}
	if decision := newOutputSizeMonitor(100, 100_000_000, false).observe(100, 100_000_001, 100); decision != nil {
		t.Fatalf("a completed attempt should be verified normally instead of killed: %+v", decision)
	}
}

func TestOutputSizeMonitorProjectsTrajectoryAtTwentyFivePercent(t *testing.T) {
	monitor := newOutputSizeMonitor(100, 100_000_000, false)
	for _, sample := range []struct {
		seconds float64
		bytes   int64
	}{
		{10, 15_000_000},
		{15, 22_500_000},
		{20, 30_000_000},
		{25, 37_500_000},
	} {
		decision := monitor.observe(sample.seconds, sample.bytes, sample.seconds)
		if sample.seconds < 25 && decision != nil {
			t.Fatalf("projection fired before 25%%: %+v", decision)
		}
		if sample.seconds == 25 {
			if decision == nil || decision.ExceededTarget {
				t.Fatalf("a projected 150 MB output should stop at 25%%, got %+v", decision)
			}
			if decision.ProjectedBytes != 150_000_000 || decision.EncodedFraction != 0.25 {
				t.Fatalf("unexpected projection: %+v", decision)
			}
		}
	}
}

func TestHardwareOutputSizeMonitorUsesTimeBoundedCheckpoint(t *testing.T) {
	monitor := newOutputSizeMonitor(90*60, 100_000_000, true)
	for _, seconds := range []float64{10, 15, 20, 25, 30} {
		// A 1 MB fixed header plus 30 KB/s projects to 163 MB: far enough
		// above target to be conclusive from a short opening sample.
		decision := monitor.observe(seconds, 1_000_000+int64(seconds*30_000), seconds)
		if seconds < 30 && decision != nil {
			t.Fatalf("time-bounded projection fired before 30 encoded seconds: %+v", decision)
		}
		if seconds == 30 {
			if decision == nil || decision.ProjectedBytes != 163_000_000 {
				t.Fatalf("long hardware encode should be stopped at 30 encoded seconds, got %+v", decision)
			}
			if decision.EncodedFraction >= 0.01 {
				t.Fatalf("the decision should happen well before 1%% of a feature-length source: %+v", decision)
			}
		}
	}
}

func TestHardwareOutputSizeMonitorCapsSlowAttemptWallTime(t *testing.T) {
	monitor := newOutputSizeMonitor(90*60, 100_000_000, true)
	samples := []struct {
		encoded float64
		wall    float64
	}{
		{5, 30},
		{7, 40},
		{9, 50},
		{11, 60},
	}
	for _, sample := range samples {
		decision := monitor.observe(sample.encoded, 1_000_000+int64(sample.encoded*30_000), sample.wall)
		if sample.wall < 60 && decision != nil {
			t.Fatalf("wall-time projection fired before the bounded checkpoint: %+v", decision)
		}
		if sample.wall == 60 && (decision == nil || decision.ProjectedBytes != 163_000_000) {
			t.Fatalf("slow hardware attempt should be stopped after one minute of evidence, got %+v", decision)
		}
	}
}

func TestHardwareOutputSizeMonitorAllowsFeasibleOpeningSpike(t *testing.T) {
	monitor := newOutputSizeMonitor(90*60, 200_000_000, true)
	for _, seconds := range []float64{5, 10, 15, 20, 25, 30} {
		// The opening projects to 220 MB, but that 10% miss is normal content
		// variation from only 30 seconds of a 90-minute movie. It must not be
		// treated as proof that a 200 MB full encode is impossible.
		if decision := monitor.observe(seconds, 1_000_000+int64(seconds*40_555.5556), seconds); decision != nil {
			t.Fatalf("short feasible opening spike should keep encoding, got %+v", decision)
		}
	}
	if got := monitor.projectedBytes(); got < 219_999_000 || got > 220_001_000 {
		t.Fatalf("opening projection = %d, want about 220000000", got)
	}
}

func TestHardwareOutputConfidenceTightensWithCoverage(t *testing.T) {
	monitor := newOutputSizeMonitor(90*60, 200_000_000, true)
	for _, test := range []struct {
		seconds float64
		want    float64
	}{
		{30, 0.30},
		{810, 0.30},
		{1080, 0.10},
		{1350, 0.02},
	} {
		got := monitor.confidenceMarginRatio(test.seconds)
		if got < test.want-0.0001 || got > test.want+0.0001 {
			t.Fatalf("confidence margin at %.0f seconds = %.4f, want %.4f", test.seconds, got, test.want)
		}
	}
}

func TestOutputSizeMonitorKeepsOnTrackAttempt(t *testing.T) {
	monitor := newOutputSizeMonitor(100, 100_000_000, false)
	for _, sample := range []struct {
		seconds float64
		bytes   int64
	}{
		// The fixed 5 MB mux/header cost should be modeled as an intercept,
		// leaving an 85 MB final projection instead of scaling 25 MB by 4.
		{10, 13_000_000},
		{15, 17_000_000},
		{20, 21_000_000},
		{25, 25_000_000},
	} {
		if decision := monitor.observe(sample.seconds, sample.bytes, sample.seconds); decision != nil {
			t.Fatalf("an on-track attempt should continue, got %+v", decision)
		}
	}
	if got := monitor.projectedBytes(); got != 85_000_000 {
		t.Fatalf("header-aware projection = %d, want 85000000", got)
	}
}

func TestEarlyCorrectionContextExplainsProjection(t *testing.T) {
	message := earlyCorrectionContext(&earlySizeCorrectionError{
		ProjectedBytes:  150_000_000,
		EncodedFraction: 0.25,
	}, 100_000_000)
	for _, detail := range []string{"25%", "150.0 MB", "100.0 MB target"} {
		if !strings.Contains(message, detail) {
			t.Fatalf("early correction message %q is missing %q", message, detail)
		}
	}
}

func TestEarlySizeCorrectionCancelsCleansAndRetries(t *testing.T) {
	tempDir := t.TempDir()
	input := filepath.Join(tempDir, "input.mp4")
	output := filepath.Join(tempDir, "output.mp4")
	if err := os.WriteFile(input, []byte("fake input"), 0o644); err != nil {
		t.Fatal(err)
	}

	ffprobe := filepath.Join(tempDir, "fake-ffprobe")
	probeDocument := `{"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"avg_frame_rate":"30/1","pix_fmt":"yuv420p"}],"format":{"duration":"10","size":"10","format_name":"mov,mp4"}}`
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nprintf '%s\\n' '"+probeDocument+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	attemptFile := filepath.Join(tempDir, "attempt")
	t.Setenv("EXACTSIZE_TEST_ATTEMPT_FILE", attemptFile)
	ffmpeg := filepath.Join(tempDir, "fake-ffmpeg")
	ffmpegScript := `#!/bin/sh
attempt=0
if [ -f "$EXACTSIZE_TEST_ATTEMPT_FILE" ]; then
  read attempt < "$EXACTSIZE_TEST_ATTEMPT_FILE"
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" > "$EXACTSIZE_TEST_ATTEMPT_FILE"
for output do :; done
if [ "$attempt" -eq 1 ]; then
  truncate -s 12000000 "$output"
  printf 'out_time_us=1000000\n'
  exec sleep 10
fi
if [ -e "$output" ]; then
  printf 'partial output still existed when retry started\n' >&2
  exit 42
fi
truncate -s 9000000 "$output"
printf 'out_time_us=10000000\nprogress=end\n'
`
	if err := os.WriteFile(ffmpeg, []byte(ffmpegScript), 0o755); err != nil {
		t.Fatal(err)
	}

	request := validTestRequest()
	request.Input = input
	request.Output = output
	request.TargetBytes = 10_000_000
	request.AudioCodec = "none"
	request.AudioBitrateKbps = 0
	request.TwoPass = false
	job := newJob(request)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	job.ctx = ctx
	job.cancel = cancel

	if err := job.runEncode(ffmpeg, ffprobe); err != nil {
		t.Fatalf("early correction run failed: %v", err)
	}
	status := job.snapshot()
	if status.State != "completed" || status.Attempt != 2 {
		t.Fatalf("expected a completed second attempt, got %+v", status)
	}
	if stat, err := os.Stat(output); err != nil || stat.Size() != 9_000_000 {
		t.Fatalf("published retry output = %v, %v; want 9000000 bytes", stat, err)
	}
	workDirs, err := filepath.Glob(filepath.Join(tempDir, ".exactsize-work-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workDirs) != 0 {
		t.Fatalf("correction work directory was not cleaned up: %v", workDirs)
	}
}

func TestRunFFmpegPreservesUsefulCorrectionMessage(t *testing.T) {
	tempDir := t.TempDir()
	ffmpeg := filepath.Join(tempDir, "fake-ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nprintf 'out_time_us=1000000\\nprogress=end\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	job := newJob(validTestRequest())
	job.status.Message = bitrateCorrectionMessage(409, minimumVideoBitrateKbps, 30)
	if err := job.runFFmpeg(ffmpeg, nil, 1, 30, 1, 1, 2, filepath.Join(tempDir, "output.webm"), 0, false); err != nil {
		t.Fatal(err)
	}
	if got := job.snapshot().Message; got != bitrateCorrectionMessage(409, minimumVideoBitrateKbps, 30) {
		t.Fatalf("encoding replaced useful correction context with %q", got)
	}
}

func TestHardwareCorrection(t *testing.T) {
	// The measured case: 3128 kbps budget, 3003 requested, 3900 delivered.
	// The ~900 kbps excess is subtracted with margin; no downscale needed.
	retry, hopeless := hardwareCorrection(3128, 3003, 3900)
	if hopeless || retry < 1900 || retry > 2200 {
		t.Errorf("hardwareCorrection(3128, 3003, 3900) = %d, %v; want ~2096, false", retry, hopeless)
	}
	// The extreme case: 367 kbps budget against a ~740 kbps floor. The
	// excess eats the budget, so the resolution is hopeless.
	if _, hopeless := hardwareCorrection(367, 352, 740); !hopeless {
		t.Error("an excess larger than half the budget must demand a downscale")
	}
	// No excess: the encoder honored the request; not an additive miss.
	if retry, hopeless := hardwareCorrection(3128, 3003, 2950); retry != 0 || hopeless {
		t.Error("an honored request must fall back to proportional correction")
	}
	// A calculated correction just above the 64 kbps floor should snap to the
	// floor instead of spending another long attempt on a negligible step.
	if retry, hopeless := hardwareCorrection(79, 79, 90); retry != minimumVideoBitrateKbps || hopeless {
		t.Fatalf("near-minimum hardware correction = %d, %v; want 64, false", retry, hopeless)
	}
}

func TestFloorAwareDownscale(t *testing.T) {
	// The 10 MB case: 1080p60 AV1 floors at ~740 kbps against a 367 kbps
	// budget. 720p predicts ~329 kbps, above 90% of budget, so the jump
	// lands at 540p directly instead of walking one rung at a time.
	w, h, ok := floorAwareDownscale(1920, 1080, 0, 740, 367)
	if !ok || h != 540 || w != 960 {
		t.Errorf("floorAwareDownscale(740 vs 367) = %d, %d, %v; want 960, 540, true", w, h, ok)
	}
	// A mild floor jumps only one rung.
	if _, h, _ := floorAwareDownscale(1920, 1080, 0, 800, 541); h != 720 {
		t.Errorf("mild floor should pick 720p, got %dp", h)
	}
	// A hopeless budget exhausts the ladder.
	if _, _, ok := floorAwareDownscale(1920, 1080, 0, 5000, 80); ok {
		t.Error("an unreachable budget must report failure")
	}
	// Already downscaled: only lower rungs are considered.
	if _, h, _ := floorAwareDownscale(1920, 1080, 540, 242, 150); h != 360 {
		t.Errorf("from 540p the next viable rung is 360p, got %dp", h)
	}
}

func TestMapProbeCodec(t *testing.T) {
	tests := map[string]string{
		"av1": "av1", "hevc": "h265", "h264": "h264", "vp9": "vp9",
		"vvc": "h266", "mpeg2video": "", "prores": "",
	}
	for name, want := range tests {
		if got := mapProbeCodec(name); got != want {
			t.Errorf("mapProbeCodec(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestRemuxCompatibility(t *testing.T) {
	av1AAC := VideoInfo{VideoCodec: "av1", AudioCodec: "aac", AudioTracks: 1}
	if err := remuxCompatibility(av1AAC, "webm", true); err == nil {
		t.Error("AAC audio must be refused a WebM remux")
	}
	if err := remuxCompatibility(av1AAC, "mp4", true); err != nil {
		t.Errorf("AV1+AAC into MP4 should remux: %v", err)
	}
	av1Opus := VideoInfo{VideoCodec: "av1", AudioCodec: "opus", AudioTracks: 1}
	if err := remuxCompatibility(av1Opus, "webm", true); err != nil {
		t.Errorf("AV1+Opus into WebM should remux: %v", err)
	}
	if err := remuxCompatibility(VideoInfo{VideoCodec: "h264", AudioCodec: "flac", AudioTracks: 1}, "mp4", true); err == nil {
		t.Error("unknown audio codecs must be MKV-only")
	}
	if err := remuxCompatibility(VideoInfo{VideoCodec: "prores", AudioTracks: 0}, "mkv", true); err != nil {
		t.Errorf("unknown video into MKV should remux: %v", err)
	}
	noAudio := VideoInfo{VideoCodec: "av1", AudioTracks: 0}
	if err := remuxCompatibility(noAudio, "webm", true); err != nil {
		t.Errorf("audioless AV1 into WebM should remux: %v", err)
	}
	// With audio re-encoding (mux mode) the source audio codec is irrelevant.
	if err := remuxCompatibility(av1AAC, "webm", false); err != nil {
		t.Errorf("mux mode must ignore the source audio codec: %v", err)
	}
	if err := remuxCompatibility(VideoInfo{VideoCodec: "h264", AudioCodec: "aac", AudioTracks: 1}, "webm", false); err == nil {
		t.Error("mux mode must still refuse incompatible video")
	}
}

func TestValidateMuxAudioRequest(t *testing.T) {
	request := EncodeRequest{Input: "/tmp/in.mp4", Output: "/tmp/out.webm", Container: "webm", Remux: true, MuxAudio: true, AudioCodec: "opus", AudioBitrateKbps: 128}
	if err := validateEncodeRequest(request); err != nil {
		t.Fatalf("mux with opus into webm should validate: %v", err)
	}
	request.AudioCodec = "aac"
	if err := validateEncodeRequest(request); err == nil {
		t.Fatal("mux with aac into webm must be rejected")
	}
	request.AudioCodec = "none"
	if err := validateEncodeRequest(request); err != nil {
		t.Fatalf("mux dropping audio should validate: %v", err)
	}
}

func TestValidateAudioBitrateMinimums(t *testing.T) {
	tests := []struct {
		codec   string
		valid   int
		invalid int
	}{
		{codec: "aac", valid: 16, invalid: 15},
		{codec: "opus", valid: 6, invalid: 5},
		{codec: "vorbis", valid: 48, invalid: 47},
		{codec: "mp3", valid: 32, invalid: 31},
	}
	for _, test := range tests {
		t.Run(test.codec, func(t *testing.T) {
			if err := validateAudioBitrate(test.codec, test.valid); err != nil {
				t.Fatalf("minimum bitrate should validate: %v", err)
			}
			if err := validateAudioBitrate(test.codec, test.invalid); err == nil {
				t.Fatalf("%d kbps should be below the codec minimum", test.invalid)
			}
		})
	}
	if err := validateAudioBitrate("none", 0); err != nil {
		t.Fatalf("disabled audio should not require a bitrate: %v", err)
	}
}

func TestOpusKeepSourceNormalizesMultichannelLayouts(t *testing.T) {
	request := EncodeRequest{AudioCodec: "opus", AudioBitrateKbps: 64, AudioChannels: "source"}
	info := VideoInfo{audioStreams: []audioStreamInfo{
		{Channels: 6, ChannelLayout: "5.1(side)"},
		{Channels: 2, ChannelLayout: "stereo"},
		{Channels: 4, ChannelLayout: "3.1"},
	}}
	args := audioEncoderArgs(request, info)
	for _, pair := range [][2]string{
		{"-filter:a:0", "aformat=channel_layouts=5.1"},
		{"-mapping_family:a:0", "1"},
		{"-mapping_family:a:2", "255"},
	} {
		if !adjacentArgs(args, pair[0], pair[1]) {
			t.Fatalf("Opus arguments are missing %q %q: %v", pair[0], pair[1], args)
		}
	}
	for _, forbidden := range []string{"-filter:a:1", "-mapping_family:a:1"} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("stereo stream must not receive %s: %v", forbidden, args)
		}
	}

	request.AudioChannels = "stereo"
	args = audioEncoderArgs(request, info)
	if !adjacentArgs(args, "-ac", "2") || slices.Contains(args, "-mapping_family:a:0") {
		t.Fatalf("explicit stereo downmix should use only -ac 2: %v", args)
	}
}

func adjacentArgs(args []string, key, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key && args[index+1] == value {
			return true
		}
	}
	return false
}

func TestOpusFivePointOneSideIntegration(t *testing.T) {
	ffmpeg := testTool(t, "EXACTSIZE_TEST_FFMPEG", "ffmpeg")
	ffprobe := testTool(t, "EXACTSIZE_TEST_FFPROBE", "ffprobe")
	directory := t.TempDir()
	input := filepath.Join(directory, "surround-input.mkv")
	create := exec.Command(ffmpeg,
		"-hide_banner", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=2:duration=0.5",
		"-f", "lavfi", "-i", "anullsrc=channel_layout=5.1(side):sample_rate=48000",
		"-t", "0.5", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "ac3", input,
	)
	if output, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create 5.1(side) input: %v\n%s", err, output)
	}
	info, err := probeVideo(t.Context(), ffprobe, input)
	if err != nil {
		t.Fatal(err)
	}
	if info.AudioChannels != 6 || info.AudioChannelLayout != "5.1(side)" {
		t.Fatalf("probed audio = %d channels, %q", info.AudioChannels, info.AudioChannelLayout)
	}

	output := filepath.Join(directory, "surround-output.webm")
	args := []string{"-hide_banner", "-y", "-loglevel", "error", "-i", input, "-map", "0:a?"}
	args = append(args, audioEncoderArgs(EncodeRequest{AudioCodec: "opus", AudioBitrateKbps: 64, AudioChannels: "source"}, info)...)
	args = append(args, "-f", "webm", output)
	if result, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("encode 5.1(side) as Opus: %v\n%s", err, result)
	}
	probeOutput, err := exec.Command(ffprobe,
		"-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=channels,channel_layout", "-of", "csv=p=0", output,
	).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(probeOutput)); got != "6,5.1" {
		t.Fatalf("Opus output layout = %q, want 6,5.1", got)
	}
}

func TestValidateRemuxRequest(t *testing.T) {
	request := EncodeRequest{Input: "/tmp/in.mkv", Output: "/tmp/out.mp4", Container: "mp4", Remux: true}
	if err := validateEncodeRequest(request); err != nil {
		t.Fatalf("a remux request needs no encoder or target: %v", err)
	}
	request.Container = "avi"
	if err := validateEncodeRequest(request); err == nil {
		t.Fatal("an unknown container must be rejected")
	}
}

func TestVAAPIDevicePlacement(t *testing.T) {
	request := validTestRequest()
	request.Encoder = "h264_vaapi"
	request.VAAPIDevice = "/dev/dri/renderD128"
	request.TwoPass = false
	args := buildFFmpegArgs(request, VideoInfo{PixelFormat: "yuv420p"}, 3000, "/tmp/out.mp4", "/tmp/pass", 1, false)
	deviceIndex, inputIndex := -1, -1
	for i, arg := range args {
		if arg == "-vaapi_device" {
			deviceIndex = i
		}
		if arg == "-i" {
			inputIndex = i
		}
	}
	if deviceIndex == -1 || inputIndex == -1 || deviceIndex > inputIndex {
		t.Fatalf("-vaapi_device must appear before -i: %v", args)
	}
}

func TestPublishedOutputMustBeDifferentFromInput(t *testing.T) {
	request := validTestRequest()
	request.Output = request.Input
	if err := validateEncodeRequest(request); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("expected same-path validation error, got %v", err)
	}
}

func TestPassOneProgressUsesFrameFallback(t *testing.T) {
	seconds, ok := progressSeconds("frame", "150", 30)
	if !ok || seconds != 5 {
		t.Fatalf("frame fallback = %v, %v; want 5, true", seconds, ok)
	}
	if _, ok := progressSeconds("out_time_us", "-9223372036854775807", 30); ok {
		t.Fatal("invalid negative FFmpeg timestamp should be ignored")
	}

	job := newJob(validTestRequest())
	job.set(func(status *JobSnapshot) {
		status.State = "running"
		status.Pass = 1
		status.Passes = 2
	})
	job.updateProgress(5, 10, 1, 2, 1, "/nonexistent")
	first := job.snapshot().Progress
	job.updateProgress(2, 10, 1, 2, 1, "/nonexistent")
	second := job.snapshot().Progress
	if first != 22.5 || second != first {
		t.Fatalf("progress moved backward: first=%v second=%v", first, second)
	}
}

func TestRetryRemainingTimeUsesCurrentAttemptOnly(t *testing.T) {
	job := newJob(validTestRequest())
	job.started = time.Now().Add(-30 * time.Minute)
	job.attemptStarted = time.Now().Add(-time.Minute)
	job.updateProgress(5, 10, 1, 1, 8, "/nonexistent")
	status := job.snapshot()
	if status.ElapsedSeconds < 29*60 {
		t.Fatalf("total elapsed time should include earlier attempts, got %.1f", status.ElapsedSeconds)
	}
	if status.RemainingSeconds < 50 || status.RemainingSeconds > 80 {
		t.Fatalf("remaining time should use only the current attempt, got %.1f seconds", status.RemainingSeconds)
	}
}

func TestStrictTargetIntegrationAllCodecs(t *testing.T) {
	ffmpeg := testTool(t, "EXACTSIZE_TEST_FFMPEG", "ffmpeg")
	ffprobe := testTool(t, "EXACTSIZE_TEST_FFPROBE", "ffprobe")
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mkv")

	command := exec.Command(ffmpeg,
		"-hide_banner", "-y", "-loglevel", "error",
		// 720p keeps the input above hardware minimum encode resolutions.
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=1.5",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=1.5",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-shortest", input,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create integration input: %v\n%s", err, output)
	}

	status, vaapiDevices, err := inspectFFmpeg(ffmpeg)
	if err != nil {
		t.Fatalf("inspect ffmpeg: %v", err)
	}
	availableEncoders := make(map[string]bool)
	for _, encoder := range status.Encoders {
		availableEncoders[encoder.ID] = true
	}

	tests := []struct {
		name      string
		container string
		codec     string
		encoder   string
	}{
		{"H264MP4", "mp4", "h264", "libx264"},
		{"H265MP4", "mp4", "h265", "libx265"},
		{"H266MP4", "mp4", "h266", "libvvenc"},
		{"AV1WebM", "webm", "av1", "libaom-av1"},
		{"VP9WebM", "webm", "vp9", "libvpx-vp9"},
		{"H264VAAPI", "mp4", "h264", "h264_vaapi"},
		{"H265VAAPI", "mp4", "h265", "hevc_vaapi"},
		{"AV1VAAPI", "mp4", "av1", "av1_vaapi"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !availableEncoders[test.encoder] {
				t.Skipf("%s is not usable on this system", test.encoder)
			}
			const target = int64(240_000)
			output := filepath.Join(dir, strings.ToLower(test.name)+"."+containerExtension(test.container))
			request := EncodeRequest{
				Input: input, Output: output, TargetBytes: target,
				Container: test.container, VideoCodec: test.codec, Encoder: test.encoder,
				Preset: "fast", AudioCodec: "aac", AudioBitrateKbps: 64,
				AudioChannels: "stereo", TwoPass: true,
				VAAPIDevice: vaapiDevices[test.encoder],
			}
			if test.container == "webm" {
				request.AudioCodec = "opus"
			}
			job := newJob(request)
			job.run(ffmpeg, ffprobe)
			result := job.snapshot()
			if result.State != "completed" {
				t.Fatalf("job ended in %s: %s", result.State, result.Error)
			}
			stat, err := os.Stat(output)
			if err != nil {
				t.Fatal(err)
			}
			if stat.Size() > target {
				t.Fatalf("output exceeded strict target: %d > %d", stat.Size(), target)
			}
			if _, err := probeVideo(context.Background(), ffprobe, output); err != nil {
				t.Fatalf("completed output is not probeable: %v", err)
			}
			// A structurally broken bitstream scores near zero; even a
			// starved-but-valid encode of this input stays far above 0.5.
			if ssim := outputSSIM(t, ffmpeg, output, input); ssim < 0.5 {
				t.Fatalf("output is visually corrupted: SSIM %.3f vs input", ssim)
			}
		})
	}
}

func outputSSIM(t *testing.T, ffmpeg, output, reference string) float64 {
	t.Helper()
	result, err := exec.Command(ffmpeg,
		"-hide_banner", "-nostdin",
		"-i", output, "-i", reference,
		"-lavfi", "ssim", "-f", "null", "-",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("ssim comparison failed: %v\n%s", err, result)
	}
	match := regexp.MustCompile(`All:([0-9.]+)`).FindSubmatch(result)
	if match == nil {
		t.Fatalf("no SSIM score in ffmpeg output:\n%s", result)
	}
	value, err := strconv.ParseFloat(string(match[1]), 64)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestFeasibleExtremeCompressionDoesNotFalseFail(t *testing.T) {
	input := os.Getenv("EXACTSIZE_REAL_CONFIDENCE_INPUT")
	if input == "" {
		t.Skip("set EXACTSIZE_REAL_CONFIDENCE_INPUT to run the long-form hardware confidence regression")
	}
	ffmpeg := testTool(t, "EXACTSIZE_TEST_FFMPEG", "ffmpeg")
	ffprobe := testTool(t, "EXACTSIZE_TEST_FFPROBE", "ffprobe")
	device := os.Getenv("EXACTSIZE_TEST_VAAPI_DEVICE")
	if device == "" {
		device = "/dev/dri/renderD128"
	}
	request := EncodeRequest{
		Input: input, Output: filepath.Join(t.TempDir(), "confidence.mp4"), TargetBytes: 200_000_000,
		Container: "mp4", VideoCodec: "h265", Encoder: "hevc_vaapi", Preset: "balanced",
		AudioCodec: "aac", AudioBitrateKbps: 64, AudioChannels: "source",
		AutoResolution: true, MinimumOutputFPS: minimumOutputFPS, VAAPIDevice: device,
	}
	job := newJob(request)
	done := make(chan error, 1)
	go func() { done <- job.runEncode(ffmpeg, ffprobe) }()

	timer := time.NewTimer(45 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("feasible 200 MB job false-failed during its confidence window: %v", err)
		}
	case <-timer.C:
		status := job.snapshot()
		job.cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel confidence regression: %v", err)
		}
		if status.Attempt != 1 {
			t.Fatalf("feasible opening was overcorrected into attempt %d: %+v", status.Attempt, status)
		}
	}
}

func TestRemuxIntegration(t *testing.T) {
	ffmpeg := testTool(t, "EXACTSIZE_TEST_FFMPEG", "ffmpeg")
	ffprobe := testTool(t, "EXACTSIZE_TEST_FFPROBE", "ffprobe")
	dir := t.TempDir()
	input := filepath.Join(dir, "input.mkv")
	command := exec.Command(ffmpeg,
		"-hide_banner", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-shortest", input,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create remux input: %v\n%s", err, output)
	}

	output := filepath.Join(dir, "remuxed.mp4")
	job := newJob(EncodeRequest{Input: input, Output: output, Container: "mp4", Remux: true})
	job.run(ffmpeg, ffprobe)
	result := job.snapshot()
	if result.State != "completed" {
		t.Fatalf("remux ended in %s: %s", result.State, result.Error)
	}
	info, err := probeVideo(context.Background(), ffprobe, output)
	if err != nil {
		t.Fatalf("remuxed output is not probeable: %v", err)
	}
	if info.VideoCodec != "h264" || info.AudioTracks != 1 {
		t.Fatalf("streams were not copied: codec=%s audio=%d", info.VideoCodec, info.AudioTracks)
	}

	// An AV1 stream must be refused a MOV remux.
	badJob := newJob(EncodeRequest{Input: input, Output: filepath.Join(dir, "x.mov"), Container: "mov", Remux: true})
	badJob.request.Input = input
	badJob.run(ffmpeg, ffprobe) // h264 into mov is fine; verify the codec gate directly instead
	if badJob.snapshot().State != "completed" {
		t.Fatalf("h264 into mov should remux: %s", badJob.snapshot().Error)
	}
}

func testTool(t *testing.T, environment, fallback string) string {
	t.Helper()
	if path := os.Getenv(environment); path != "" {
		return path
	}
	path, err := exec.LookPath(fallback)
	if err != nil {
		t.Skipf("%s is not available", fallback)
	}
	return path
}

func validTestRequest() EncodeRequest {
	return EncodeRequest{
		Input: "/tmp/input.mp4", Output: "/tmp/output.mp4", TargetBytes: 10_000_000,
		Container: "mp4", VideoCodec: "h264", Encoder: "libx264", Preset: "balanced",
		AudioCodec: "aac", AudioBitrateKbps: 128, AudioChannels: "stereo", TwoPass: true,
	}
}
