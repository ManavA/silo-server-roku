package playback

import (
	"fmt"
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
	}
	// Audio remains cached for this transport. Its segment boundaries and encoder
	// priming must survive video recovery without borrowing video ordinals.
}

// OpenMediaSegment waits for an audio-first request after reconstruction. Audio
// demand temporarily lets the producer advance without changing video recovery
// or its download high-water mark.
func (s *TranscodeSession) OpenMediaSegment(name string) (*SegmentLease, error) {
	number, err := ParseAudioSegmentNumber(name)
	s.mu.Lock()
	wait := err == nil && s.opts.SeparateAudioHLS && number >= s.opts.StartSegmentNumber && number-s.opts.StartSegmentNumber < maxSyntheticManifestSegments
	if wait {
		s.audioSegmentWaiters++
	}
	s.mu.Unlock()
	if !wait {
		return s.OpenSegment(name)
	}
	defer func() { s.mu.Lock(); s.audioSegmentWaiters--; s.mu.Unlock() }()
	return s.WaitForOpenSegment(name, 60*time.Second)
}

func (s *TranscodeSession) waitingForAudioSegment() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioSegmentWaiters > 0
}
