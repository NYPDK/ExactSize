package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type VideoInfo struct {
	Path               string  `json:"path"`
	Name               string  `json:"name"`
	Size               int64   `json:"size"`
	Duration           float64 `json:"duration"`
	Width              int     `json:"width"`
	Height             int     `json:"height"`
	FPS                float64 `json:"fps"`
	VideoCodec         string  `json:"videoCodec"`
	PixelFormat        string  `json:"pixelFormat"`
	AudioCodec         string  `json:"audioCodec"`
	AudioChannels      int     `json:"audioChannels"`
	AudioChannelLayout string  `json:"audioChannelLayout,omitempty"`
	AudioSampleRate    int     `json:"audioSampleRate"`
	AudioTracks        int     `json:"audioTracks"`
	SubtitleTracks     int     `json:"subtitleTracks"`
	Format             string  `json:"format"`
	audioStreams       []audioStreamInfo
}

type audioStreamInfo struct {
	Channels      int
	ChannelLayout string
}

type ffprobeDocument struct {
	Streams []struct {
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		RFrameRate    string `json:"r_frame_rate"`
		AvgFrameRate  string `json:"avg_frame_rate"`
		PixelFormat   string `json:"pix_fmt"`
		Channels      int    `json:"channels"`
		ChannelLayout string `json:"channel_layout"`
		SampleRate    string `json:"sample_rate"`
	} `json:"streams"`
	Format struct {
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		FormatName string `json:"format_name"`
	} `json:"format"`
}

func probeVideo(parent context.Context, ffprobe, path string) (VideoInfo, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return VideoInfo{}, errors.New("select an input video first")
	}
	stat, err := os.Stat(path)
	if err != nil {
		return VideoInfo{}, fmt.Errorf("input video cannot be opened: %w", err)
	}
	if stat.IsDir() {
		return VideoInfo{}, errors.New("the selected input is a directory, not a video")
	}

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-show_entries", "format=duration,size,format_name:stream=codec_type,codec_name,width,height,r_frame_rate,avg_frame_rate,pix_fmt,channels,channel_layout,sample_rate",
		"-of", "json",
		path,
	)
	output, err := command.Output()
	if err != nil {
		return VideoInfo{}, errors.New("ffprobe could not read this file as a video")
	}

	var document ffprobeDocument
	if err := json.Unmarshal(output, &document); err != nil {
		return VideoInfo{}, fmt.Errorf("read video metadata: %w", err)
	}
	duration, _ := strconv.ParseFloat(document.Format.Duration, 64)
	if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return VideoInfo{}, errors.New("the video has no usable duration")
	}

	info := VideoInfo{
		Path:     path,
		Name:     filepath.Base(path),
		Size:     stat.Size(),
		Duration: duration,
		Format:   document.Format.FormatName,
	}
	for _, stream := range document.Streams {
		switch stream.CodecType {
		case "video":
			if info.VideoCodec == "" {
				info.VideoCodec = stream.CodecName
				info.Width = stream.Width
				info.Height = stream.Height
				info.PixelFormat = stream.PixelFormat
				info.FPS = parseRate(stream.AvgFrameRate)
				if info.FPS == 0 {
					info.FPS = parseRate(stream.RFrameRate)
				}
			}
		case "audio":
			info.AudioTracks++
			info.audioStreams = append(info.audioStreams, audioStreamInfo{
				Channels:      stream.Channels,
				ChannelLayout: stream.ChannelLayout,
			})
			sampleRate, _ := strconv.Atoi(stream.SampleRate)
			if sampleRate > info.AudioSampleRate {
				info.AudioSampleRate = sampleRate
			}
			if info.AudioCodec == "" {
				info.AudioCodec = stream.CodecName
				info.AudioChannels = stream.Channels
				info.AudioChannelLayout = stream.ChannelLayout
			}
		case "subtitle":
			info.SubtitleTracks++
		}
	}
	if info.VideoCodec == "" {
		return VideoInfo{}, errors.New("the selected file does not contain a video stream")
	}
	return info, nil
}

// OutputBreakdown separates encoded stream payload from the bytes added by
// the container. It is measured from packet sizes after an oversized attempt,
// so audio VBR drift and mux overhead are never mistaken for video bitrate.
type OutputBreakdown struct {
	TotalBytes   int64
	VideoBytes   int64
	AudioBytes   int64
	OtherBytes   int64
	MuxBytes     int64
	VideoPackets int64
	AudioPackets int64
}

func probeOutputBreakdown(parent context.Context, ffprobe, path string) (OutputBreakdown, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return OutputBreakdown{}, fmt.Errorf("measure encoded output: %w", err)
	}

	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-show_entries", "packet=codec_type,size",
		"-of", "csv=p=0",
		path,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return OutputBreakdown{}, fmt.Errorf("measure encoded output: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return OutputBreakdown{}, fmt.Errorf("start output measurement: %w", err)
	}

	breakdown, parseErr := parseOutputBreakdown(stdout, stat.Size())
	waitErr := command.Wait()
	if ctx.Err() != nil {
		return OutputBreakdown{}, ctx.Err()
	}
	if waitErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = waitErr.Error()
		}
		return OutputBreakdown{}, fmt.Errorf("measure encoded output: %s", detail)
	}
	if parseErr != nil {
		return OutputBreakdown{}, parseErr
	}
	return breakdown, nil
}

func parseOutputBreakdown(reader io.Reader, totalBytes int64) (OutputBreakdown, error) {
	breakdown := OutputBreakdown{TotalBytes: totalBytes}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), ",", 3)
		if len(fields) < 2 {
			continue
		}
		size, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if err != nil || size < 0 {
			continue
		}
		switch strings.TrimSpace(fields[0]) {
		case "video":
			breakdown.VideoBytes += size
			breakdown.VideoPackets++
		case "audio":
			breakdown.AudioBytes += size
			breakdown.AudioPackets++
		default:
			breakdown.OtherBytes += size
		}
	}
	if err := scanner.Err(); err != nil {
		return OutputBreakdown{}, fmt.Errorf("read encoded packet sizes: %w", err)
	}
	if breakdown.VideoBytes <= 0 {
		return OutputBreakdown{}, errors.New("ffprobe found no encoded video packets")
	}
	payloadBytes := breakdown.VideoBytes + breakdown.AudioBytes + breakdown.OtherBytes
	if payloadBytes < totalBytes {
		breakdown.MuxBytes = totalBytes - payloadBytes
	}
	return breakdown, nil
}

func parseRate(rate string) float64 {
	parts := strings.Split(rate, "/")
	if len(parts) == 2 {
		numerator, _ := strconv.ParseFloat(parts[0], 64)
		denominator, _ := strconv.ParseFloat(parts[1], 64)
		if denominator != 0 {
			return numerator / denominator
		}
	}
	value, _ := strconv.ParseFloat(rate, 64)
	return value
}
