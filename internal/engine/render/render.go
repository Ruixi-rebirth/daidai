package render

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daidai/internal/core"
)

const (
	maxRenderInputBytes = 1 << 20
	maxRenderPNGBytes   = core.MaxImageBytes
)

// RenderCard 将 Markdown 文本通过 Typst 编译为排版优雅的高清 PNG 卡片图
func RenderCard(ctx context.Context, md string) ([]byte, error) {
	trimmed := strings.TrimSpace(md)
	if trimmed == "" {
		return nil, fmt.Errorf("empty markdown text")
	}
	if len(trimmed) > maxRenderInputBytes {
		return nil, fmt.Errorf("markdown input exceeds %d byte limit", maxRenderInputBytes)
	}

	typstBody := MarkdownToTypst([]byte(trimmed))

	doc := fmt.Sprintf(`#set page(
  width: 780pt,
  height: auto,
  margin: (x: 28pt, top: 24pt, bottom: 28pt),
  fill: rgb("#f6f8fa")
)

#set text(
  font: ("Maple Mono", "Source Han Sans SC"),
  size: 13.5pt,
  fill: rgb("#1f2328"),
  lang: "zh"
)

#set par(justify: false, leading: 0.85em)

#show heading: it => block(below: 10pt, above: 14pt)[
  #text(weight: "bold", fill: rgb("#0969da"))[#it.body]
]

#show raw.where(block: true): it => block(
  fill: rgb("#24292e"),
  inset: 12pt,
  radius: 8pt,
  width: 100%%
)[
  #set text(fill: rgb("#f0f6fc"), size: 11.5pt)
  #it
]

#show raw.where(block: false): it => box(
  fill: rgb("#eff1f3"),
  inset: (x: 4pt, y: 2pt),
  radius: 4pt
)[
  #set text(fill: rgb("#bf3989"), size: 12pt)
  #it
]

#block(
  width: 100%%,
  fill: rgb("#ffffff"),
  inset: (x: 24pt, top: 22pt, bottom: 24pt),
  radius: 12pt,
  stroke: 0.8pt + rgb("#e1e4e8")
)[
%s
]
`, typstBody)

	tmpDir, err := os.MkdirTemp("", "daidai-card-*")
	if err != nil {
		return nil, fmt.Errorf("create private render directory failed: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	typPath := filepath.Join(tmpDir, "card.typ")
	pngPath := filepath.Join(tmpDir, "card.png")

	if err := os.WriteFile(typPath, []byte(doc), 0600); err != nil {
		return nil, fmt.Errorf("write typst file failed: %w", err)
	}

	cmd := exec.CommandContext(ctx, "typst", "compile", "--root", tmpDir, typPath, pngPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	output := core.NewLimitedBuffer(64 << 10)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(output.String())
		if output.Truncated() {
			detail += " [compiler output truncated]"
		}
		return nil, fmt.Errorf("typst compile failed: %s (%w)", detail, err)
	}

	pngFile, err := os.Open(pngPath)
	if err != nil {
		return nil, fmt.Errorf("open rendered png failed: %w", err)
	}
	pngBytes, readErr := core.ReadBounded(pngFile, maxRenderPNGBytes)
	closeErr := pngFile.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read rendered png failed: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close rendered png failed: %w", closeErr)
	}
	if len(pngBytes) < 8 || !bytes.HasPrefix(pngBytes, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, fmt.Errorf("rendered output is not a valid PNG")
	}

	return pngBytes, nil
}
