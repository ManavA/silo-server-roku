package playback

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSeparateAudioHLSAACPreservesSoundTimingWithoutVideoReordering(t *testing.T) {
	if testing.Short() {
		t.Skip("real FFmpeg audio integration test")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is unavailable")
	}
	ffprobe := ffprobePathFromFFmpeg(ffmpeg)
	if _, err := exec.LookPath(ffprobe); err != nil {
		t.Skip("ffprobe is unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	run := func(binary string, args ...string) []byte {
		t.Helper()
		data, err := exec.CommandContext(ctx, binary, args...).Output()
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(binary), err)
		}
		return data
	}
	source := filepath.Join(t.TempDir(), "source.mkv")
	run(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=24", "-f", "lavfi", "-i", "anoisesrc=sample_rate=48000:seed=1234", "-t", "3", "-c:v", "libx264", "-bf", "0", "-g", "48", "-c:a", "eac3", "-ac", "6", source)
	dir := t.TempDir()
	session, err := StartTranscode(ctx, TranscodeOpts{InputPath: source, OutputDir: dir, FFmpegPath: ffmpeg, TargetCodecVideo: "copy", SourceVideoCodec: "h264", TargetCodecAudio: "aac", SourceAudioChannels: 6, TargetAudioChannels: 2, SeparateAudioHLS: true, SegmentDuration: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	select {
	case <-session.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	videoPackets := func(path string) map[string]float64 {
		var report struct {
			Packets []struct {
				PTS  string `json:"pts_time"`
				Hash string `json:"data_hash"`
			} `json:"packets"`
		}
		if err := json.Unmarshal(run(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_packets", "-show_data_hash", "sha256", "-show_entries", "packet=pts_time,data_hash", "-of", "json", path), &report); err != nil {
			t.Fatal(err)
		}
		result := map[string]float64{}
		for _, packet := range report.Packets {
			pts, err := strconv.ParseFloat(packet.PTS, 64)
			if err != nil {
				t.Fatal(err)
			}
			result[packet.Hash] = pts
		}
		return result
	}
	original := videoPackets(source)
	videoShift := math.NaN()
	for hash, pts := range videoPackets(filepath.Join(dir, "stream.m3u8")) {
		if before, exists := original[hash]; exists {
			videoShift = pts - before
			break
		}
	}
	if math.IsNaN(videoShift) {
		t.Fatal("copied video packet identity is unavailable")
	}
	firstAudioPTS := func(path string) float64 {
		var report struct {
			Frames []struct {
				PTS string `json:"pts_time"`
			} `json:"frames"`
		}
		if err := json.Unmarshal(run(ffprobe, "-v", "error", "-select_streams", "a:0", "-show_frames", "-show_entries", "frame=pts_time", "-of", "json", path), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Frames) == 0 {
			t.Fatal("audio rendition has no decoded frames")
		}
		pts, err := strconv.ParseFloat(report.Frames[0].PTS, 64)
		if err != nil {
			t.Fatal(err)
		}
		return pts
	}
	pcm := func(path string) []float64 {
		data := run(ffmpeg, "-v", "error", "-i", path, "-map", "0:a:0", "-ac", "1", "-ar", "48000", "-t", "2", "-c:a", "pcm_f32le", "-f", "f32le", "-")
		result := make([]float64, len(data)/4)
		for i := range result {
			result[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(data[i*4 : i*4+4])))
		}
		return result
	}
	sourcePTS := firstAudioPTS(source)
	outputPath := filepath.Join(dir, "audio.m3u8")
	outputPTS := firstAudioPTS(outputPath)
	before, after := pcm(source), pcm(outputPath)
	const sourceIndex, window = 8192, 1024
	nominal := sourceIndex + int(math.Round((sourcePTS+videoShift-outputPTS)*48000))
	bestIndex, bestScore := -1, -1.0
	for index := nominal - 2048; index <= nominal+2048; index++ {
		if index < 0 || index+window > len(after) || sourceIndex+window > len(before) {
			continue
		}
		dot, left, right := 0.0, 0.0, 0.0
		for offset := 0; offset < window; offset++ {
			a, b := before[sourceIndex+offset], after[index+offset]
			dot += a * b
			left += a * a
			right += b * b
		}
		if left == 0 || right == 0 {
			continue
		}
		score := dot / math.Sqrt(left*right)
		if score > bestScore {
			bestIndex, bestScore = index, score
		}
	}
	if bestIndex < 0 || bestScore < 0.5 {
		t.Fatalf("converted audio cannot be correlated: %.3f", bestScore)
	}
	audioShift := outputPTS + float64(bestIndex-sourceIndex)/48000 - sourcePTS
	if math.Abs(audioShift-videoShift) > 0.005 {
		t.Fatalf("sound/video timestamp shifts differ: %.6f/%.6f (sourcePTS %.6f, outputPTS %.6f, sample shift %d)", audioShift, videoShift, sourcePTS, outputPTS, bestIndex-sourceIndex)
	}
}
