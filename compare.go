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
