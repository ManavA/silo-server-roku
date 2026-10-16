package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Eyevinn/mp4ff/mp4"
)

const maxDVFragmentBytes = 128 << 20

// ConvertDV7Fragment converts only RPUs and removes the enhancement layer. The
// copied base samples keep their decode times, composition offsets and durations;
// passing raw HEVC through a second muxer would lose those source timestamps.
// Input must be a single, clear, video-only fMP4 fragment and its original init.
func ConvertDV7Fragment(ctx context.Context, toolPath string, initData, fragmentData []byte) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(initData) > 2<<20 || len(fragmentData) > maxDVFragmentBytes {
		return nil, nil, fmt.Errorf("Dolby Vision fragment exceeds conversion limit")
	}
	initFile, err := mp4.DecodeFile(bytes.NewReader(initData))
	if err != nil || initFile.Init == nil {
		return nil, nil, fmt.Errorf("invalid Dolby Vision initialization")
	}
	init := initFile.Init
	if init.Moov == nil || len(init.Moov.Traks) != 1 || init.Moov.Mvex == nil || init.Moov.Mvex.Trex == nil {
		return nil, nil, fmt.Errorf("Dolby Vision conversion requires one fragmented video track")
	}
	track := init.Moov.Trak
	if track == nil || track.Tkhd == nil || track.Mdia == nil || track.Mdia.Minf == nil || track.Mdia.Minf.Stbl == nil || track.Mdia.Minf.Stbl.Stsd == nil {
		return nil, nil, fmt.Errorf("Dolby Vision track configuration is unavailable")
	}
	entry := track.Mdia.Minf.Stbl.Stsd.HvcX
	if entry == nil || entry.HvcC == nil || entry.HvcC.LengthSizeMinusOne != 3 || entry.DoViConfig == nil {
		return nil, nil, fmt.Errorf("Dolby Vision configuration is unavailable")
	}
	config := entry.DoViConfig
	if config.DVProfile != 7 || !config.RPUPresentFlag || !config.BLPresentFlag || config.DVLevel < 1 || config.DVLevel > 13 {
		return nil, nil, fmt.Errorf("unsupported Dolby Vision conversion source")
	}
	file, err := mp4.DecodeFile(bytes.NewReader(fragmentData))
	if err != nil || len(file.Segments) != 1 || len(file.Segments[0].Fragments) != 1 {
		return nil, nil, fmt.Errorf("invalid Dolby Vision media fragment")
	}
	fragment := file.Segments[0].Fragments[0]
	if fragment.Moof == nil || fragment.Moof.Mfhd == nil || len(fragment.Moof.Trafs) != 1 || fragment.Moof.Trafs[0].Tfhd == nil || fragment.Moof.Trafs[0].Tfhd.TrackID != init.Moov.Trak.Tkhd.TrackID {
		return nil, nil, fmt.Errorf("Dolby Vision fragment track differs from initialization")
	}
	samples, err := fragment.GetFullSamples(init.Moov.Mvex.Trex)
	if err != nil || len(samples) == 0 {
		return nil, nil, fmt.Errorf("Dolby Vision fragment has no readable samples")
	}
	var annexB bytes.Buffer
	startCode := []byte{0, 0, 0, 1}
	for _, array := range entry.HvcC.NaluArrays {
		for _, nalu := range array.Nalus {
			annexB.Write(startCode)
			annexB.Write(nalu)
		}
	}
	sourceNALs := make([][][]byte, len(samples))
	rpuCount := 0
	for i, sample := range samples {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		nalus, err := lengthPrefixedHEVCNALs(sample.Data)
		if err != nil {
			return nil, nil, err
		}
		sourceNALs[i] = nalus
		for _, nalu := range nalus {
			annexB.Write(startCode)
			annexB.Write(nalu)
			if nalu[0]>>1&63 == 62 {
				rpuCount++
			}
		}
	}
	if rpuCount == 0 {
		return nil, nil, fmt.Errorf("Dolby Vision fragment contains no RPU")
	}
	converted, err := convertDV7AnnexB(ctx, toolPath, annexB.Bytes())
	if err != nil {
		return nil, nil, err
	}
	rpus := dolbyVisionRPUs(converted)
	if len(rpus) != rpuCount {
		return nil, nil, fmt.Errorf("Dolby Vision conversion changed RPU count")
	}
	output, err := mp4.CreateFragment(fragment.Moof.Mfhd.SequenceNumber, init.Moov.Trak.Tkhd.TrackID)
	if err != nil {
		return nil, nil, err
	}
	rpuIndex := 0
	for i, sample := range samples {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		var data bytes.Buffer
		for _, nalu := range sourceNALs[i] {
			switch nalu[0] >> 1 & 63 {
			case 63:
				continue
			case 62:
				nalu = rpus[rpuIndex]
				rpuIndex++
			default:
				if (uint16(nalu[0]&1)<<5)|uint16(nalu[1]>>3) != 0 {
					return nil, nil, fmt.Errorf("unsupported Dolby Vision enhancement-layer layout")
				}
			}
			if err := binary.Write(&data, binary.BigEndian, uint32(len(nalu))); err != nil {
				return nil, nil, err
			}
			data.Write(nalu)
		}
		sample.Data = data.Bytes()
		sample.Size = uint32(len(sample.Data))
		output.AddFullSample(sample)
	}
	children := entry.Children[:0]
	for _, child := range entry.Children {
		if _, ok := child.(*mp4.DoViConfigurationBox); !ok {
			children = append(children, child)
		}
	}
	entry.Children = children
	entry.SetType(VideoSampleEntryDVH1)
	entry.AddChild(mp4.CreateDoViConfigurationBox(config.DVVersionMajor, config.DVVersionMinor, 8, config.DVLevel, true, false, true, 1))
	var outInit, outFragment bytes.Buffer
	if err := init.Encode(&outInit); err != nil {
		return nil, nil, err
	}
	if err := output.Encode(&outFragment); err != nil {
		return nil, nil, err
	}
	return outInit.Bytes(), outFragment.Bytes(), nil
}

func lengthPrefixedHEVCNALs(data []byte) ([][]byte, error) {
	var nalus [][]byte
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, fmt.Errorf("truncated HEVC NAL length")
		}
		size := uint64(binary.BigEndian.Uint32(data[:4]))
		data = data[4:]
		if size < 2 || size > uint64(len(data)) {
			return nil, fmt.Errorf("invalid HEVC NAL length")
		}
		nalus = append(nalus, data[:int(size)])
		data = data[int(size):]
	}
	return nalus, nil
}

func convertDV7AnnexB(ctx context.Context, toolPath string, data []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "silo-dv-convert-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input, output := filepath.Join(dir, "input.hevc"), filepath.Join(dir, "output.hevc")
	if err := os.WriteFile(input, data, 0600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, toolPath, "-m", "2", "convert", "--discard", "-i", input, "-o", output)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("Dolby Vision converter failed: %w", err)
	}
	return readBoundedHLSFile(output, maxDVFragmentBytes)
}

func dolbyVisionRPUs(data []byte) [][]byte {
	// Normalize Annex B's three- and four-byte start codes before selecting RPUs.
	var rpus [][]byte
	start := -1
	for i := 0; i+3 <= len(data); {
		length := 0
		if i+4 <= len(data) && bytes.Equal(data[i:i+4], []byte{0, 0, 0, 1}) {
			length = 4
		} else if bytes.Equal(data[i:i+3], []byte{0, 0, 1}) {
			length = 3
		}
		if length == 0 {
			i++
			continue
		}
		if start >= 0 && i-start >= 2 && data[start]>>1&63 == 62 {
			rpus = append(rpus, data[start:i])
		}
		start = i + length
		i = start
	}
	if start >= 0 && len(data)-start >= 2 && data[start]>>1&63 == 62 {
		rpus = append(rpus, data[start:])
	}
	return rpus
}

// readBoundedHLSFile bounds allocations even if a producer replaces or grows
// the file between opening and reading it.
func readBoundedHLSFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return nil, fmt.Errorf("HLS file exceeds read limit or is empty")
	}
	return data, nil
}
