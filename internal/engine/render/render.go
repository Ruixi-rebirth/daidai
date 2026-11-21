package render

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"daidai/internal/core"
)

const (
	maxRenderInputBytes = 1 << 20
	maxRenderPNGBytes   = core.MaxImageBytes
)

//go:embed code-theme.tmTheme
var codeTheme []byte

type cardStyle struct {
	textColor        string
	paragraphLeading string
	headingColor     string
	headingBelow     string
	headingAbove     string
	codeRadius       string
	inlineCodeFill   string
	cardFill         string
	cardRadius       string
	cardStroke       string
	canvasColor      string
}

var standardCardStyle = cardStyle{
	textColor:        "#1f2328",
	paragraphLeading: "0.85em",
	headingColor:     "#0969da",
	headingBelow:     "10pt",
	headingAbove:     "14pt",
	codeRadius:       "8pt",
	inlineCodeFill:   "#eff1f3",
	cardFill:         "#ffffff",
	cardRadius:       "10pt",
	cardStroke:       "0.5pt + rgb(\"#e1e4e8\")",
	canvasColor:      "#f4f5f8",
}

var memoryProfileCardStyle = cardStyle{
	textColor:        "#493a3e",
	paragraphLeading: "0.95em",
	headingColor:     "#ad527b",
	headingBelow:     "9pt",
	headingAbove:     "15pt",
	codeRadius:       "9pt",
	inlineCodeFill:   "#f8eaf0",
	cardFill:         "#fffdf9",
	cardRadius:       "10pt",
	cardStroke:       "0.6pt + rgb(\"#e9d7d5\")",
	canvasColor:      "#f8f0ed",
}

// RenderCard 将 Markdown 文本通过 Typst 编译为排版优雅的高清 PNG 卡片图
func RenderCard(ctx context.Context, md string) ([]byte, error) {
	return renderMarkdownCard(ctx, md, standardCardStyle)
}

// RenderMemoryProfileCard uses a warmer palette and wider spacing for a
// personal impression and memory list.
func RenderMemoryProfileCard(ctx context.Context, md string) ([]byte, error) {
	return renderMarkdownCard(ctx, md, memoryProfileCardStyle)
}

func renderMarkdownCard(ctx context.Context, md string, style cardStyle) ([]byte, error) {
	trimmed := strings.TrimSpace(md)
	if trimmed == "" {
		return nil, fmt.Errorf("empty markdown text")
	}
	if len(trimmed) > maxRenderInputBytes {
		return nil, fmt.Errorf("markdown input exceeds %d byte limit", maxRenderInputBytes)
	}

	typstBody := MarkdownToTypst([]byte(trimmed))

	doc := fmt.Sprintf(`#set page(
  width: 680pt,
  height: auto,
  margin: 0pt,
  fill: none
)

#set text(
  font: ("Twitter Color Emoji", "Maple Mono", "Source Han Sans SC"),
  size: 18pt,
  fill: rgb("%s"),
  lang: "zh"
)

#set par(justify: false, leading: %s)
#set raw(theme: "code-theme.tmTheme")
#show raw: set text(font: ("Maple Mono", "Source Han Sans SC"), size: 1em / 0.8)

#show heading: it => block(below: %s, above: %s)[
  #text(weight: "bold", fill: rgb("%s"))[#it.body]
]

#show raw.where(block: true): it => block(
  fill: rgb("#17212b"),
  inset: 12pt,
  radius: %s,
  width: 100%%
)[
  #set text(fill: rgb("#eef2f6"), size: 16pt)
  #it
]

#show raw.where(block: false): it => box(
  fill: rgb("%s"),
  inset: (x: 4pt, y: 2pt),
  radius: 4pt
)[
  #set raw(theme: none)
  #set text(fill: rgb("#93316a"), size: 17pt)
  #it
]

#block(
  width: 100%%,
  fill: rgb("%s"),
  inset: (x: 16pt, top: 14pt, bottom: 16pt),
  radius: %s,
  stroke: %s
)[
%s
]
`, style.textColor, style.paragraphLeading, style.headingBelow, style.headingAbove, style.headingColor, style.codeRadius, style.inlineCodeFill, style.cardFill, style.cardRadius, style.cardStroke, typstBody)
	return renderTypst(ctx, doc, style.canvasColor)
}

// HasMathFormulas checks whether markdown text contains real LaTeX math expressions
// outside code blocks, while strictly excluding currency amounts like $100.
func HasMathFormulas(md string) bool {
	trimmed := strings.TrimSpace(md)
	if trimmed == "" || !strings.Contains(trimmed, "$") {
		return false
	}
	_, mathList := extractAndProtectMath([]byte(trimmed))
	return len(mathList) > 0
}

// ShouldRenderCard decides from the content's size, math content, and structure whether an
// image is easier to scan than a short chat reply. Explicit image tools can
// still render regardless of this preference.
func ShouldRenderCard(md string) bool {
	trimmed := strings.TrimSpace(md)
	if trimmed == "" {
		return false
	}
	if HasMathFormulas(trimmed) {
		return true
	}
	runeCount := utf8.RuneCountInString(trimmed)
	nonEmptyLines, headings, listItems, tableLines := 0, 0, 0, 0
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonEmptyLines++
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			headings++
		}
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "• ") {
			listItems++
		}
		if strings.HasPrefix(line, "|") && strings.Contains(line[1:], "|") {
			tableLines++
		}
	}
	if tableLines >= 3 || headings >= 2 || (headings >= 1 && runeCount >= 60) || (nonEmptyLines >= 4 && listItems >= 3) {
		return true
	}
	return runeCount >= 80 || (runeCount >= 60 && nonEmptyLines >= 3)
}

func renderTypst(ctx context.Context, doc string, canvasColor string) ([]byte, error) {
	return renderTypstAssets(ctx, doc, nil, canvasColor)
}

func renderTypstAssets(ctx context.Context, doc string, assets map[string][]byte, canvasColor string) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("rendering requires a context")
	}
	tmpDir, err := os.MkdirTemp("", "daidai-card-*")
	if err != nil {
		return nil, fmt.Errorf("create private render directory failed: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	for name, data := range assets {
		if filepath.Base(name) != name || name == "." || name == ".." {
			return nil, fmt.Errorf("invalid render asset name")
		}
		if err := os.WriteFile(filepath.Join(tmpDir, name), data, 0600); err != nil {
			return nil, fmt.Errorf("write render asset: %w", err)
		}
	}
	typPath := filepath.Join(tmpDir, "card.typ")
	pngPath := filepath.Join(tmpDir, "card.png")
	if err := os.WriteFile(filepath.Join(tmpDir, "code-theme.tmTheme"), codeTheme, 0600); err != nil {
		return nil, fmt.Errorf("write code theme: %w", err)
	}

	if err := os.WriteFile(typPath, []byte(doc), 0600); err != nil {
		return nil, fmt.Errorf("write typst file failed: %w", err)
	}

	cmd := core.CommandProcessGroup(ctx, "typst", "compile", "--root", tmpDir, typPath, pngPath)
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

	// 后处理：添加极简衬底画板 (Studio Canvas) + 3D 浮起投影阴影
	if canvasColor != "" {
		if shadowed, err := applyShadowEffect(ctx, tmpDir, pngPath, canvasColor); err == nil {
			pngBytes = shadowed
		} else {
			slog.Warn("render: apply shadow effect failed, falling back to flat card", "err", err)
		}
	}

	config, err := png.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("read rendered PNG dimensions: %w", err)
	}
	if err := core.ValidateImageDimensions(config.Width, config.Height); err != nil {
		return nil, fmt.Errorf("rendered image dimensions are invalid: %w", err)
	}

	return pngBytes, nil
}

// applyShadowEffect 使用 ImageMagick 为透明背景圆角卡片添加紧凑画板与 3D 浮起投影阴影。
// 失败时返回错误并回退，保证基础卡片能够正常发送。
func applyShadowEffect(ctx context.Context, tmpDir, srcPath, canvasColor string) ([]byte, error) {
	bin := "magick"
	if _, err := exec.LookPath("magick"); err != nil {
		if _, err := exec.LookPath("convert"); err == nil {
			bin = "convert"
		} else {
			return nil, fmt.Errorf("neither magick nor convert found in PATH")
		}
	}

	outPath := filepath.Join(tmpDir, "card_shadow.png")

	// 极简画板衬底 (16px)，紧凑节约空间，阴影柔和自然，在 QQ 全屏黑底看图时完整呈现 3D 悬浮感
	cmd := core.CommandProcessGroup(ctx, bin,
		srcPath,
		"-bordercolor", canvasColor, "-border", "16",
		"(", "+clone", "-background", "#0f172a", "-shadow", "55x12+6+8", ")",
		"+swap", "-background", canvasColor, "-layers", "merge", "+repage",
		outPath,
	)
	cmd.WaitDelay = 10 * time.Second
	errBuf := core.NewLimitedBuffer(16 << 10)
	cmd.Stderr = errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("imagemagick shadow failed: %s (%w)", strings.TrimSpace(errBuf.String()), err)
	}

	outFile, err := os.Open(outPath)
	if err != nil {
		return nil, fmt.Errorf("open shadow png: %w", err)
	}
	out, readErr := core.ReadBounded(outFile, maxRenderPNGBytes)
	closeErr := outFile.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(out) < 8 || !bytes.HasPrefix(out, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, fmt.Errorf("shadow output is not a valid PNG")
	}
	return out, nil
}
