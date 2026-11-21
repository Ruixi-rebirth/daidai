package chat

import (
	"context"
	"encoding/json"
	"fmt"

	"daidai/internal/core"
	"daidai/internal/engine/render"
	"daidai/internal/entity"
)

// RenderCard renders structured command output for channels where an image is
// easier to scan than a long Markdown reply. Callers can fall back to text.
func (c *Cortex) RenderCard(ctx context.Context, event *entity.SensoryEvent, content string) ([]byte, error) {
	if c.hands == nil {
		return nil, fmt.Errorf("tool registry is not configured")
	}
	args, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return nil, fmt.Errorf("encode card content: %w", err)
	}
	toolCtx := entity.WithSystemToolAccess(ctx, "render_card")
	if event != nil {
		toolCtx = entity.WithSensoryEvent(toolCtx, event)
	}
	res, err := c.hands.Execute(toolCtx, "render_card", args)
	if err != nil {
		return nil, err
	}
	if res == nil || len(res.ImageData) == 0 {
		if res != nil && res.Text != "" {
			return nil, fmt.Errorf("render card returned no image: %s", res.Text)
		}
		return nil, fmt.Errorf("render card returned no image")
	}
	return res.ImageData, nil
}

// RenderCardPages turns long replies into a small, ordered set of readable cards.
func (c *Cortex) RenderCardPages(ctx context.Context, event *entity.SensoryEvent, content string, maxPages int) ([][]byte, error) {
	if maxPages < 1 {
		return nil, fmt.Errorf("image page limit must be positive")
	}
	pageRunes := 1400
	chunks := core.SplitMarkdownChunks(content, pageRunes)
	for len(chunks) > maxPages && pageRunes < maxAssistantReplyRunes {
		pageRunes += 300
		chunks = core.SplitMarkdownChunks(content, pageRunes)
	}
	if len(chunks) > maxPages {
		return nil, fmt.Errorf("long reply requires %d image pages; limit is %d", len(chunks), maxPages)
	}
	images := make([][]byte, 0, len(chunks))
	for index, chunk := range chunks {
		pageContent := chunk
		if len(chunks) > 1 {
			pageContent = fmt.Sprintf("### [%d / %d]\n\n%s", index+1, len(chunks), chunk)
		}
		image, err := c.RenderCard(ctx, event, pageContent)
		if err != nil {
			return nil, fmt.Errorf("render reply page %d of %d: %w", index+1, len(chunks), err)
		}
		images = append(images, image)
	}
	return images, nil
}

// ShouldRenderCard applies the deterministic presentation policy without an
// extra model call. The same policy is used for ordinary replies and notices.
func (c *Cortex) ShouldRenderCard(_ context.Context, content string) bool {
	return render.ShouldRenderCard(content)
}
