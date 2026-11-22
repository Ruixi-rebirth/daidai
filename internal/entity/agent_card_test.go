package entity

import (
	"context"
	"strings"
	"testing"
)

type cardTestCognition struct {
	shouldRender bool
	renderCalls  int
	pageCount    int
}

func (*cardTestCognition) Think(context.Context, *SensoryEvent) (*Utterance, error) { return nil, nil }
func (*cardTestCognition) MindStatus() string                                       { return "" }
func (*cardTestCognition) ModelList() string                                        { return "" }
func (*cardTestCognition) ModelUsage() string                                       { return "" }
func (c *cardTestCognition) ShouldRenderCard(context.Context, string) bool          { return c.shouldRender }
func (c *cardTestCognition) RenderCardPages(_ context.Context, _ *SensoryEvent, _ string, pages int) ([][]byte, error) {
	c.renderCalls++
	c.pageCount = pages
	return [][]byte{[]byte("rendered page")}, nil
}

func TestRenderAutomaticReplyCardKeepsExistingMediaAndSourcesAligned(t *testing.T) {
	content := strings.Repeat("长回复内容。", 20)
	cognition := &cardTestCognition{shouldRender: true}
	agent := &SuperAgent{Cognition: cognition}
	utterance := &Utterance{
		Text:         content,
		FallbackText: "已有文字回退",
		Images:       [][]byte{[]byte("existing image")},
		ImageSources: []string{"existing source"},
		CardData:     []byte("legacy card"),
	}

	agent.renderAutomaticReplyCard(context.Background(), &SensoryEvent{Platform: "qq", Role: RolePartner}, utterance)

	if cognition.renderCalls != 1 || cognition.pageCount == 0 {
		t.Fatalf("card renderer was not called with the reply's page count: %+v", cognition)
	}
	if utterance.Text != "" || !utterance.InlineImages || utterance.InlineNonTextImageCount != 2 || len(utterance.Images) != 3 {
		t.Fatalf("automatic card render lost existing media: %+v", utterance)
	}
	if len(utterance.ImageSources) != len(utterance.Images) || utterance.ImageSources[0] != "existing source" || utterance.ImageSources[1] != "" || utterance.ImageSources[2] != content {
		t.Fatalf("image source metadata is not aligned: %+v", utterance.ImageSources)
	}
	if !strings.Contains(utterance.FallbackText, "已有文字回退") || !strings.Contains(utterance.FallbackText, content) {
		t.Fatal("text fallback must preserve prior fallback and the rendered reply")
	}
}

func TestRenderAutomaticReplyCardHonorsPresentationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		platform  string
		groupTalk bool
		fixed     bool
		textOnly  bool
	}{
		{name: "unsupported platform", platform: "cli"},
		{name: "group interjection", platform: "qq", groupTalk: true},
		{name: "fixed output", platform: "qq", fixed: true},
		{name: "text preference", platform: "qq", textOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cognition := &cardTestCognition{shouldRender: true}
			agent := &SuperAgent{Cognition: cognition}
			utterance := &Utterance{Text: strings.Repeat("长回复内容。", 20), FixedPresentation: tc.fixed, SuppressAutoCard: tc.textOnly}
			agent.renderAutomaticReplyCard(context.Background(), &SensoryEvent{Platform: tc.platform, GroupInterjection: tc.groupTalk}, utterance)
			if cognition.renderCalls != 0 || utterance.Text == "" {
				t.Fatalf("render crossed a presentation boundary: calls=%d utterance=%+v", cognition.renderCalls, utterance)
			}
		})
	}
}
