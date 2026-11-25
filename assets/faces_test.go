package assets

import (
	"bytes"
	"image/png"
	"testing"
)

func TestAllBundledFacesLoadValidPNG(t *testing.T) {
	if len(BundledFaces) != 34 {
		t.Fatalf("expected 34 bundled faces, got %d", len(BundledFaces))
	}
	for _, name := range BundledFaces {
		data, canonical, ok := ReadBundledSmallFace(name)
		if !ok || canonical != name || len(data) == 0 {
			t.Fatalf("failed to load bundled face %q", name)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
			t.Fatalf("bundled face %q is not a valid PNG: %v", name, err)
		}
	}
	if _, canonical, ok := ReadBundledSmallFace("笑"); !ok || canonical != "开心" {
		t.Fatalf("expected alias '笑' -> '开心', got ok=%v canonical=%q", ok, canonical)
	}
	if _, _, ok := ReadBundledSmallFace("不存在的表情"); ok {
		t.Fatal("unexpected match for nonexistent face")
	}
}
