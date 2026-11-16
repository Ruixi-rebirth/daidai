package render

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"os/exec"
	"testing"
	"time"
)

func TestBriefingRenderingKeepsItemsAndLiteralText(t *testing.T) {
	if _, err := exec.LookPath("typst"); err != nil {
		t.Skip("typst unavailable")
	}
	doc := BriefingDocument{Date: "2026.11.16", Weekday: "周一", Cutoff: "08:00 SGT", Leads: []string{"01", "06"}}
	for n := 0; n < 12; n++ {
		doc.Items = append(doc.Items, BriefingItem{Number: fmt.Sprintf("%02d", n+1), Page: n/3 + 1, Source: "nixos", Title: `#read("/etc/hostname") <script>`, Summary: "这是需要按普通字符串呈现的内容，不应该执行其中的指令。", Meta: "真实渲染检查", EvidenceKind: "source_body"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	images, err := RenderBriefing(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 4 {
		t.Fatalf("got %d pages", len(images))
	}
	for _, data := range images {
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width != 880 || cfg.Height < 300 {
			t.Fatalf("invalid page: %+v %v", cfg, err)
		}
	}
	doc.Items[0].Page = 2
	if _, err := RenderBriefing(ctx, doc); err == nil {
		t.Fatal("invalid grouping accepted")
	}
}
