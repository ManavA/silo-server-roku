package playback

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ParseAudioSegmentNumber keeps audio ordinals separate from video recovery:
// an audio segment with the same number can begin at a different source time.
func ParseAudioSegmentNumber(name string) (int, error) {
	if len(name) > 64 || !strings.HasPrefix(name, "audio_") || !strings.HasSuffix(name, ".ts") {
		return 0, fmt.Errorf("invalid audio segment")
	}
	number, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "audio_"), ".ts"))
	if err != nil || number < 0 || name != fmt.Sprintf("audio_%05d.ts", number) {
		return 0, fmt.Errorf("invalid audio segment")
	}
	return number, nil
}

// ReportMediaSegmentDownloadedForGeneration accounts for a completed response
// on its own rendition and only in the generation that served the file.
func (s *TranscodeSession) ReportMediaSegmentDownloadedForGeneration(name string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.segmentGeneration {
		return
	}
	s.reportMediaSegmentDownloadedLocked(name)
}

// ReportMediaSegmentDownloadedForGenerationToken is the authenticated relay's
// counterpart. A delayed acknowledgement cannot advance a replacement job.
func (s *TranscodeSession) ReportMediaSegmentDownloadedForGenerationToken(name, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "" || token != s.segmentGenerationTokenLocked() {
		return
	}
	s.reportMediaSegmentDownloadedLocked(name)
}

func (s *TranscodeSession) reportMediaSegmentDownloadedLocked(name string) {
	if number, err := ParseSegmentNumber(name); err == nil {
		s.reportSegmentDownloadedLocked(number)
		return
	}
	number, err := ParseAudioSegmentNumber(name)
	if err != nil || !s.opts.SeparateAudioHLS {
		return
	}
	s.lastCompletedAudioSegment = max(s.lastCompletedAudioSegment, number)
	s.scheduleSeparateAudioPruneLocked()
}

func (s *TranscodeSession) scheduleSeparateAudioPruneLocked() {
	if s.opts.SegmentRetentionSeconds <= 0 || s.audioPruneRunning || s.restarting != nil || s.lastCompletedAudioSegment-s.audioPruneHighWater < segmentPruneHysteresis {
		return
	}
	s.audioPruneRunning = true
	go s.pruneSeparateAudioSegments(s.segmentGeneration, s.lastCompletedAudioSegment)
}

func (s *TranscodeSession) pruneSeparateAudioSegments(generation uint64, completed int) {
	more := false
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if generation != s.segmentGeneration {
			return
		}
		s.audioPruneRunning = false
		if !more {
			s.audioPruneHighWater = completed
		}
		s.scheduleSeparateAudioPruneLocked()
	}()
	s.mu.Lock()
	opts, dir := s.opts, s.outputDir
	valid := generation == s.segmentGeneration && s.restarting == nil
	s.mu.Unlock()
	if !valid {
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, "audio.m3u8"))
	if err != nil {
		return
	}
	// Both playlists use the same duration grammar. Normalize only the local
	// audio filename prefix; no audio ordinal enters the video recovery policy.
	timeline, err := parseManifestTimeline([]byte(strings.ReplaceAll(string(data), "\naudio_", "\nseg_")))
	if err != nil || len(timeline.entries) == 0 {
		return
	}
	floor := timeline.entries[0].number
	retained := 0.0
	for i := len(timeline.entries) - 1; i >= 0; i-- {
		entry := timeline.entries[i]
		if entry.number > completed {
			continue
		}
		retained += entry.duration
		floor = entry.number
		if retained >= float64(opts.SegmentRetentionSeconds) {
			break
		}
	}
	startupEnd := opts.StartSegmentNumber + startupSegmentRequirement(opts)
	guard := time.Duration(2*max(1, opts.SegmentDuration))*time.Second + 30*time.Second
	removed := 0
	for _, entry := range timeline.entries {
		if entry.number >= floor || entry.number < startupEnd {
			continue
		}
		path := filepath.Join(dir, fmt.Sprintf("audio_%05d.ts", entry.number))
		info, err := os.Stat(path)
		if err != nil || time.Since(info.ModTime()) < guard {
			continue
		}
		s.mu.Lock()
		if generation != s.segmentGeneration || s.restarting != nil {
			s.mu.Unlock()
			return
		}
		err = os.Remove(path)
		s.mu.Unlock()
		if err == nil {
			removed++
		}
		if removed >= segmentPruneBatchSize {
			more = true
			return
		}
	}
}
