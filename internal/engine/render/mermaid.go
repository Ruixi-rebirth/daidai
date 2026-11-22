package render

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"daidai/internal/core"
)

const (
	maxMermaidRenderWindow = time.Minute
)

type mermaidBlock struct {
	start  int
	end    int
	source string
}

// RenderMermaidBlocks converts Mermaid fenced blocks into private inline asset
// references. Parsing is line based and only treats actual Markdown fences as
// diagrams; Mermaid source is never sent to the chat channel.
func RenderMermaidBlocks(ctx context.Context, markdown string) (string, []core.InlineAsset, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	blocks := findMermaidBlocks(markdown)
	if len(blocks) == 0 {
		return markdown, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, maxMermaidRenderWindow)
	defer cancel()
	if _, err := exec.LookPath("mmdc"); err != nil {
		return replaceFailedMermaidBlocks(markdown, blocks), nil, fmt.Errorf("mermaid CLI mmdc is unavailable: %w", err)
	}
	var assets []core.InlineAsset
	var output strings.Builder
	position := 0
	var renderErrors []string
	for index, block := range blocks {
		output.WriteString(markdown[position:block.start])
		asset, err := renderMermaid(ctx, block.source)
		if err != nil {
			output.WriteString("\n[流程图生成失败，源码已省略]\n\n")
			renderErrors = append(renderErrors, fmt.Sprintf("diagram %d: %v", index+1, err))
			position = block.end
			continue
		}
		assets = append(assets, asset)
		output.WriteString("\n" + asset.Placeholder + "\n\n")
		position = block.end
	}
	output.WriteString(markdown[position:])
	if len(renderErrors) > 0 {
		return output.String(), assets, fmt.Errorf("mermaid rendering incomplete: %s", strings.Join(renderErrors, "; "))
	}
	return output.String(), assets, nil
}

func renderMermaid(ctx context.Context, source string) (core.InlineAsset, error) {
	lowerSource := strings.ToLower(source)
	if strings.Contains(lowerSource, "http:") || strings.Contains(lowerSource, "https:") || strings.Contains(lowerSource, "file:") || strings.Contains(lowerSource, "data:") {
		return core.InlineAsset{}, fmt.Errorf("external Mermaid resources are not allowed")
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return core.InlineAsset{}, fmt.Errorf("create diagram reference: %w", err)
	}
	id := hex.EncodeToString(idBytes[:])
	dir, err := os.MkdirTemp("", "daidai-mermaid-")
	if err != nil {
		return core.InlineAsset{}, fmt.Errorf("create temporary directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("render: failed to remove temporary Mermaid directory", "path", dir, "err", err)
		}
	}()
	input := filepath.Join(dir, "diagram.mmd")
	outputPath := filepath.Join(dir, "diagram.png")
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(input, []byte(source), 0600); err != nil {
		return core.InlineAsset{}, fmt.Errorf("write Mermaid source: %w", err)
	}
	configData, _ := json.Marshal(map[string]any{"securityLevel": "strict", "startOnLoad": false, "htmlLabels": false})
	if err := os.WriteFile(config, configData, 0600); err != nil {
		return core.InlineAsset{}, fmt.Errorf("write Mermaid config: %w", err)
	}
	command := core.CommandProcessGroup(ctx, "mmdc", "--input", input, "--output", outputPath, "--configFile", config, "--quiet")
	command.Dir = dir
	if commandOutput, err := command.CombinedOutput(); err != nil {
		return core.InlineAsset{}, fmt.Errorf("mermaid CLI failed: %w: %s", err, core.TruncateRunes(strings.TrimSpace(string(commandOutput)), 300, ""))
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return core.InlineAsset{}, fmt.Errorf("stat rendered PNG: %w", err)
	}
	if info.Size() <= 0 || info.Size() > core.MaxImageBytes {
		return core.InlineAsset{}, fmt.Errorf("rendered PNG size %d is outside the accepted range", info.Size())
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return core.InlineAsset{}, fmt.Errorf("read rendered PNG: %w", err)
	}
	dimensions, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return core.InlineAsset{}, fmt.Errorf("rendered output is not a PNG: %w", err)
	}
	if err := core.ValidateImageDimensions(dimensions.Width, dimensions.Height); err != nil {
		return core.InlineAsset{}, err
	}
	alt := "流程图"
	placeholder := "![" + alt + "](daidai-inline://" + id + ")"
	return core.InlineAsset{Placeholder: placeholder, Reference: "daidai-inline://" + id, Alt: alt, Fallback: "[流程图]", SemanticText: source, Data: data}, nil
}

func findMermaidBlocks(markdown string) []mermaidBlock {
	var blocks []mermaidBlock
	lineStart := 0
	for lineStart < len(markdown) {
		lineEnd := strings.IndexByte(markdown[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(markdown)
		} else {
			lineEnd += lineStart
		}
		fence, length, info, ok := parseFence(markdown[lineStart:lineEnd])
		if !ok {
			lineStart = nextLine(lineEnd, len(markdown))
			continue
		}
		bodyStart := nextLine(lineEnd, len(markdown))
		bodyEnd := bodyStart
		closeEnd := len(markdown)
		closed := false
		for bodyEnd < len(markdown) {
			end := strings.IndexByte(markdown[bodyEnd:], '\n')
			if end < 0 {
				end = len(markdown)
			} else {
				end += bodyEnd
			}
			closeFence, closeLength, closeInfo, closes := parseFence(markdown[bodyEnd:end])
			if closes && closeFence == fence && closeLength >= length && strings.TrimSpace(closeInfo) == "" {
				closed = true
				closeEnd = nextLine(end, len(markdown))
				break
			}
			bodyEnd = nextLine(end, len(markdown))
		}
		if !strings.EqualFold(strings.TrimSpace(info), "mermaid") {
			if !closed {
				break
			}
			lineStart = closeEnd
			continue
		}
		sourceEnd := bodyEnd
		if sourceEnd > bodyStart && markdown[sourceEnd-1] == '\n' {
			sourceEnd--
		}
		blocks = append(blocks, mermaidBlock{start: lineStart, end: closeEnd, source: markdown[bodyStart:sourceEnd]})
		if !closed {
			break
		}
		lineStart = closeEnd
	}
	return blocks
}

func parseFence(line string) (byte, int, string, bool) {
	index := 0
	for index < len(line) && index < 4 && line[index] == ' ' {
		index++
	}
	if index > 3 || index >= len(line) || (line[index] != '`' && line[index] != '~') {
		return 0, 0, "", false
	}
	fence := line[index]
	start := index
	for index < len(line) && line[index] == fence {
		index++
	}
	if index-start < 3 {
		return 0, 0, "", false
	}
	return fence, index - start, strings.TrimSpace(line[index:]), true
}

func nextLine(end, total int) int {
	if end >= 0 && end < total {
		return end + 1
	}
	return total
}

func replaceFailedMermaidBlocks(markdown string, blocks []mermaidBlock) string {
	var output strings.Builder
	position := 0
	for _, block := range blocks {
		output.WriteString(markdown[position:block.start])
		output.WriteString("\n[流程图生成失败，源码已省略]\n\n")
		position = block.end
	}
	output.WriteString(markdown[position:])
	return output.String()
}
