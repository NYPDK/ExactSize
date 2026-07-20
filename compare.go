package main

import (
	"fmt"
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
