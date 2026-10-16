package playback

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSeparateAudioHLSRecoveryKeepsAudioURIMeaning(t *testing.T) {
	if testing.Short() {
		t.Skip("real FFmpeg recovery integration test")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	source := filepath.Join(t.TempDir(), "source.mkv")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=24", "-f", "lavfi", "-i", "anoisesrc=sample_rate=48000:seed=1234", "-t", "8", "-c:v", "libx264", "-g", "48", "-c:a", "eac3", "-ac", "6", source).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	for _, origin := range []float64{0, 2} {
		t.Run(time.Duration(origin*float64(time.Second)).String(), func(t *testing.T) {
			dir := t.TempDir()
			s, err := StartTranscode(ctx, TranscodeOpts{InputPath: source, OutputDir: dir, FFmpegPath: ffmpeg, TargetCodecVideo: "copy", SourceVideoCodec: "h264", TargetCodecAudio: "aac", SourceAudioChannels: 6, SeparateAudioHLS: true, SegmentDuration: 2, SeekSeconds: origin})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			select {
			case <-s.Done():
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			first := filepath.Join(dir, "audio_00000.ts")
			before, err := os.ReadFile(first)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Restart(ctx, origin+4, 2); err != nil {
				t.Fatal(err)
			}
			select {
			case <-s.Done():
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			after, err := os.ReadFile(first)
			if err != nil {
				t.Fatal("original audio URI disappeared", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("audio URI changed bytes or timestamps across video recovery")
			}
		})
	}
}
