package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
)

func TestDV7FragmentConversionPreservesSampleTimingAndBaseLayer(t *testing.T) {
	init := mp4.CreateEmptyInit()
	init.AddEmptyTrack(90000, "video", "und")
	entry := mp4.CreateVisualSampleEntryBox("dvh1", 1920, 1080, &mp4.HvcCBox{DecConfRec: hevc.DecConfRec{ConfigurationVersion: 1, LengthSizeMinusOne: 3}})
	entry.AddChild(mp4.CreateDoViConfigurationBox(1, 0, 7, 6, true, true, true, 6))
	init.Moov.Trak.Mdia.Minf.Stbl.Stsd.AddChild(entry)
	var initData bytes.Buffer
	if err := init.Encode(&initData); err != nil {
		t.Fatal(err)
	}
	base := []byte{0x26, 1, 0x11, 0x22}
	oldRPU := []byte{0x7c, 1, 7}
	el := []byte{0x7e, 1, 0x33}
	data := dvFragmentTestSample(base, oldRPU, el)
	original := mp4.FullSample{Sample: mp4.NewSample(0x02000000, 3003, uint32(len(data)), -1001), DecodeTime: 900000, Data: data}
	frag, err := mp4.CreateFragment(7, 1)
	if err != nil {
		t.Fatal(err)
	}
	frag.AddFullSample(original)
	var fragmentData bytes.Buffer
	if err := frag.Encode(&fragmentData); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(t.TempDir(), "dovi_tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '\\000\\000\\000\\001\\174\\001\\201' > \"$8\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	convertedInit, convertedFragment, err := ConvertDV7Fragment(t.Context(), tool, initData.Bytes(), fragmentData.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	decodedInit, err := mp4.DecodeFile(bytes.NewReader(convertedInit))
	if err != nil {
		t.Fatal(err)
	}
	config := decodedInit.Init.Moov.Trak.Mdia.Minf.Stbl.Stsd.HvcX.DoViConfig
	if config == nil || config.DVProfile != 8 || config.DVBLSignalCompatibilityID != 1 || config.ELPresentFlag || !config.RPUPresentFlag {
		t.Fatalf("converted configuration = %+v", config)
	}
	decoded, err := mp4.DecodeFile(bytes.NewReader(convertedFragment))
	if err != nil {
		t.Fatal(err)
	}
	samples, err := decoded.Segments[0].Fragments[0].GetFullSamples(decodedInit.Init.Moov.Mvex.Trex)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("sample count = %d", len(samples))
	}
	sample := samples[0]
	if sample.DecodeTime != 900000 || sample.CompositionTimeOffset != -1001 || sample.Dur != 3003 || sample.Flags != 0x02000000 {
		t.Fatalf("sample timing changed: %+v", sample.Sample)
	}
	if !bytes.Equal(sample.Data, dvFragmentTestSample(base, []byte{0x7c, 1, 0x81})) {
		t.Fatalf("base layer or RPU replacement is wrong: %x", sample.Data)
	}
}

func dvFragmentTestSample(nalus ...[]byte) []byte {
	var data bytes.Buffer
	for _, nalu := range nalus {
		if err := binary.Write(&data, binary.BigEndian, uint32(len(nalu))); err != nil {
			panic(err)
		}
		data.Write(nalu)
	}
	return data.Bytes()
}

func TestDV7HLSRecipeSurvivesReconstruction(t *testing.T) {
	opts := TranscodeOpts{SessionID: "dv7-fixture", TargetCodecVideo: "copy", SeparateAudioHLS: true, DV7ToDV81: true}
	card := NewRecipeCard(1, "profile", 42, "", opts)
	restored := RecipeCardFromClaims(ptr(card.ToClaims())).TranscodeOpts(t.TempDir(), "ffmpeg", nil)
	if !restored.DV7ToDV81 {
		t.Fatal("reconstructed transport lost DV conversion")
	}
	if card.CopyFMP4RecipeVersion != "4" {
		t.Fatal("older separate-audio worker can accept the new byte recipe")
	}
}

func TestDV7HLSRejectsAnUnvalidatedToolBeforeOpeningMedia(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dovi_tool"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	opts := TranscodeOpts{OutputDir: t.TempDir(), FFmpegPath: "/unavailable/ffmpeg", TargetCodecVideo: "copy", SourceVideoCodec: "hevc", SeparateAudioHLS: true, DV7ToDV81: true, VideoSampleEntry: VideoSampleEntryDVH1}
	_, err := StartTranscode(t.Context(), opts)
	if err == nil || !strings.Contains(err.Error(), "conversion tool") {
		t.Fatalf("invalid converter reached media execution: %v", err)
	}
}

func TestDV7FragmentConversionHonorsCancellationBeforeReadingMedia(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := ConvertDV7Fragment(ctx, "unavailable", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled conversion = %v", err)
	}
}

func TestLengthPrefixedHEVCNALsRejectsMalformedLengths(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "truncated prefix", data: []byte{0, 0, 0}},
		{name: "missing header", data: []byte{0, 0, 0, 1, 0x26}},
		{name: "truncated payload", data: []byte{0, 0, 0, 3, 0x26, 1}},
		{name: "maximum unsigned length", data: []byte{255, 255, 255, 255, 0x26, 1}},
		{name: "signed integer overflow", data: []byte{128, 0, 0, 0, 0x26, 1}},
		{name: "trailing partial prefix", data: []byte{0, 0, 0, 2, 0x26, 1, 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := lengthPrefixedHEVCNALs(test.data); err == nil {
				t.Fatal("malformed HEVC NAL length was accepted")
			}
		})
	}
	valid := []byte{0, 0, 0, 2, 0x26, 1, 0, 0, 0, 3, 0x7c, 1, 7}
	nalus, err := lengthPrefixedHEVCNALs(valid)
	if err != nil || len(nalus) != 2 || !bytes.Equal(nalus[0], []byte{0x26, 1}) || !bytes.Equal(nalus[1], []byte{0x7c, 1, 7}) {
		t.Fatalf("valid bounded NALs = %x, %v", nalus, err)
	}
}
