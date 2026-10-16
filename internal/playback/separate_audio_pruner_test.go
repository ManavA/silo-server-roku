package playback

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSeparateAudioHLSPrunesItsOwnDownloadedTimeline(t *testing.T) {
	dir := t.TempDir()
	var manifest strings.Builder
	manifest.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n")
	old := time.Now().Add(-time.Minute)
	for number := 0; number < 20; number++ {
		name := fmt.Sprintf("audio_%05d.ts", number)
		fmt.Fprintf(&manifest, "#EXTINF:1.000,\n%s\n", name)
		writePrunerTestFile(t, filepath.Join(dir, name), []byte("audio"), old)
	}
	if err := os.WriteFile(filepath.Join(dir, "audio.m3u8"), []byte(manifest.String()), 0600); err != nil {
		t.Fatal(err)
	}
	session := &TranscodeSession{outputDir: dir, opts: TranscodeOpts{SeparateAudioHLS: true, TargetCodecVideo: "copy", SegmentDuration: 2, SegmentRetentionSeconds: 4}}
	session.ReportMediaSegmentDownloadedForGeneration("audio_00019.ts", 0)
	waitForPrunerFileMissing(t, filepath.Join(dir, "audio_00015.ts"))
	for _, number := range []int{0, 1, 16, 17, 18, 19} {
		assertPrunerFileExists(t, filepath.Join(dir, fmt.Sprintf("audio_%05d.ts", number)))
	}
	if session.LastRequestedSegment() != 0 {
		t.Fatal("audio download advanced the video ordinal")
	}
}

func TestDV7HLSPrunesPrivateSourceWithConvertedVideo(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, dv7SourceDirectory)
	if err := os.Mkdir(raw, 0700); err != nil {
		t.Fatal(err)
	}
	opts := TranscodeOpts{TargetCodecVideo: "copy", SeparateAudioHLS: true, DV7ToDV81: true, SegmentDuration: 2, SegmentRetentionSeconds: 10}
	var manifest strings.Builder
	manifest.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n")
	old := time.Now().Add(-time.Minute)
	for number := 0; number <= 15; number++ {
		name := fmt.Sprintf("seg_%05d.m4s", number)
		fmt.Fprintf(&manifest, "#EXTINF:2.000,\n%s\n", name)
		writePrunerTestFile(t, filepath.Join(dir, name), []byte("converted"), old)
		writePrunerTestFile(t, filepath.Join(raw, name), []byte("source"), old)
	}
	writePrunerTestFile(t, filepath.Join(dir, "stream.m3u8"), []byte(manifest.String()), old)
	session := &TranscodeSession{outputDir: dir, opts: opts, lastPruneFloor: -1}
	session.ReportSegmentDownloaded(15)
	waitForPrunerFileMissing(t, filepath.Join(raw, "seg_00009.m4s"))
	assertPrunerFileMissing(t, filepath.Join(dir, "seg_00009.m4s"))
	assertPrunerFileExists(t, filepath.Join(raw, "seg_00015.m4s"))
}
