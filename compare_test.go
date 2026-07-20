package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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

func TestStoryboardSpecFor(t *testing.T) {
	cases := []struct {
		name               string
		duration           float64
		width, height      int
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
	cases := []struct {
		name          string
		input, output float64
		want          float64
	}{
		{name: "normal case: take min and subtract fudge", input: 84.3, output: 84.25, want: 84.2},
		{name: "tiny durations must clamp to 0.1", input: 0.05, output: 9, want: 0.1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareTimelineDuration(tc.input, tc.output); got != tc.want {
				t.Fatalf("compareTimelineDuration(%v, %v) = %v, want %v", tc.input, tc.output, got, tc.want)
			}
		})
	}
}

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
