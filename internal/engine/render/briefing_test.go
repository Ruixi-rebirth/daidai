package render

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBriefingRenderingKeepsItemsAndLiteralText(t *testing.T) {
	if _, err := exec.LookPath("typst"); err != nil {
		t.Skip("typst unavailable")
	}
	doc := BriefingDocument{Date: "2026.11.16", Weekday: "周一", Cutoff: "08:00 SGT", Leads: []string{"01", "06"}}
	for n := 0; n < 20; n++ {
		title := strings.Repeat("最长标题验证", 10)
		if n == 0 {
			title = `#read("/etc/hostname") <script>`
		}
		source := []string{"nixos", "hn", "github", "v2ex"}[n/5]
		doc.Items = append(doc.Items, BriefingItem{Number: fmt.Sprintf("%02d", n+1), Source: source, Title: title, Summary: strings.Repeat("正文排版验证", 20), Meta: "真实渲染检查", EvidenceKind: "source_body"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	card, err := RenderBriefing(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(card))
	if err != nil || cfg.Width != 1720 || cfg.Height < 3000 {
		t.Fatalf("invalid complete long image: %+v %v", cfg, err)
	}
	doc.Items = append(doc.Items, doc.Items[0])
	if _, err := RenderBriefing(ctx, doc); err == nil {
		t.Fatal("more than twenty items accepted")
	}
}
