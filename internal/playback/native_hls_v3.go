package playback

import (
	"fmt"
	"strings"
)

// PlanUsesMPEGTSV3 reports the frozen HLS packaging recipe. The transformation
// keeps route identity and executor capability checks tied to the actual bytes.
func PlanUsesMPEGTSV3(plan *PlanV3) bool {
	if plan == nil || plan.Delivery != DeliveryRemuxHLSV3 {
		return false
	}
	for _, transformation := range plan.Transformations {
		if transformation.Executor == ExecutorServerV3 && transformation.Name == TransformationHLSMPEGTSV3 && transformation.RecipeVersion == TransformationHLSMPEGTSRecipeVersionV3 {
			return true
		}
	}
	return false
}

func nativeHLSPackagingV3(request StartRequestV3) string {
	for _, recipe := range []struct{ feature, name string }{{ClientNativeHLSSeparateAudioV3, TransformationHLSSeparateAudioV3}, {ClientNativeHLSMPEGTSV3, TransformationHLSMPEGTSV3}} {
		if HasFeatureV3(request.ClientFeatures, recipe.feature) && deliverySupportsFeatureV3(request, DeliveryClassHLSV3, recipe.feature) {
			return recipe.name
		}
	}
	return ""
}

// PlanUsesSeparateAudioHLSV3 reports the frozen audio rendition recipe.
func PlanUsesSeparateAudioHLSV3(plan *PlanV3) bool {
	if plan == nil || plan.Delivery != DeliveryRemuxHLSV3 {
		return false
	}
	for _, transformation := range plan.Transformations {
		if transformation.Executor == ExecutorServerV3 && transformation.Name == TransformationHLSSeparateAudioV3 && transformation.RecipeVersion == TransformationHLSSeparateAudioRecipeVersionV3 {
			return true
		}
	}
	return false
}

// NativeHLSMasterAttributesV3 describes the selected output, rather than a DV
// source whose metadata the recipe may strip. Unknown codec detail is omitted.
func NativeHLSMasterAttributesV3(plan *PlanV3, audioProfile string) (string, string) {
	if plan == nil {
		return "", ""
	}
	videoRange := "SDR"
	switch plan.EffectiveRecipe.DynamicRange {
	case DynamicRangeHDR10V3, DynamicRangeHDR10PlusV3, DynamicRangeDolbyVisionV3:
		videoRange = "PQ"
	case DynamicRangeHLGV3:
		videoRange = "HLG"
	}
	videoCodec := ""
	if plan.EffectiveRecipe.VideoCodec == "hevc" {
		if plan.EffectiveRecipe.DynamicRange == DynamicRangeDolbyVisionV3 && plan.Source.DVLevel > 0 {
			profile := plan.Source.DVProfile
			if PlanConvertsDV7ToDV81V3(plan) {
				profile = 8
			}
			videoCodec = fmt.Sprintf("dvh1.%02d.%02d", profile, plan.Source.DVLevel)
		} else if plan.Source.VideoLevel > 0 {
			profile := "1.6"
			if plan.Source.BitDepth > 8 {
				profile = "2.4"
			}
			videoCodec = fmt.Sprintf("hvc1.%s.L%d.B0", profile, plan.Source.VideoLevel)
		}
	}
	audioCodec := ""
	switch plan.EffectiveRecipe.AudioCodec {
	case "eac3":
		audioCodec = "ec-3"
	case "ac3":
		audioCodec = "ac-3"
	case "aac":
		converted := false
		for _, transformation := range plan.Transformations {
			if transformation.Name == TransformationAudioToAACV3 {
				converted = true
			}
		}
		if converted || strings.EqualFold(strings.TrimSpace(audioProfile), "LC") {
			audioCodec = "mp4a.40.2"
		}
	}
	if audioCodec == "" {
		return "", videoRange
	}
	return videoCodec + "," + audioCodec, videoRange
}
