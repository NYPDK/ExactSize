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
