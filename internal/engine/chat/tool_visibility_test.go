package chat

import (
	"context"
	"slices"
	"testing"

	"daidai/internal/core"
	"daidai/internal/engine/vision"
	"daidai/internal/entity"
	"daidai/internal/tool"
)

func TestConversationSpecificToolVisibility(t *testing.T) {
	cases := []struct {
		name  string
		event *entity.SensoryEvent
		want  bool
	}{
		{name: "QQ partner private", event: &entity.SensoryEvent{Platform: "qq", Role: entity.RolePartner}, want: true},
		{name: "QQ group", event: &entity.SensoryEvent{Platform: "qq", IsGroup: true, Role: entity.RolePartner}},
		{name: "collaborator private", event: &entity.SensoryEvent{Platform: "qq", Role: entity.RoleCollaborator}},
		{name: "Telegram private", event: &entity.SensoryEvent{Platform: "telegram", Role: entity.RolePartner}},
		{name: "missing context"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entity.ToolAllowedInConversation("manage_access", tc.event); got != tc.want {
				t.Fatalf("ToolAllowedInConversation() = %t, want %t", got, tc.want)
			}
		})
	}
	if !entity.ToolAllowedInConversation("manage_memory", &entity.SensoryEvent{Platform: "telegram", Role: entity.RoleGuest}) {
		t.Fatal("access filtering must not hide unrelated tools")
	}
	if !entity.ToolAllowedInConversation("manage_group_expressions", &entity.SensoryEvent{Platform: "telegram", ChannelID: "group", IsGroup: true, Role: entity.RolePartner}) {
		t.Fatal("group expression settings should be available to the partner in a group")
	}
	if entity.ToolAllowedInConversation("manage_group_expressions", &entity.SensoryEvent{Platform: "qq", Role: entity.RolePartner}) {
		t.Fatal("group expression settings should be hidden outside groups")
	}
	if entity.ToolAllowedInConversation("manage_geek_briefing", &entity.SensoryEvent{Role: entity.RoleCollaborator}) {
		t.Fatal("briefing management should be hidden from non-partners")
	}
}

func TestImageToolAdvertisesOnlyReadableSources(t *testing.T) {
	engine := vision.NewEngine(vision.Config{APIKey: "fixture"})
	hands := tool.NewHandsWithConfig(tool.HandsConfig{Vision: engine})
	cortex := NewCortex(CortexConfig{LLM: &preparedViewModel{}, Hands: hands, Vision: engine})
	images := []*core.Attachment{{URL: "fixture:image"}}
	for _, tc := range []struct {
		current, quoted bool
		want            []string
	}{
		{current: true, want: []string{entity.CurrentImageSource}},
		{quoted: true, want: []string{entity.QuotedImageSource}},
		{current: true, quoted: true, want: []string{entity.CurrentImageSource, entity.QuotedImageSource}},
		{},
	} {
		event := &entity.SensoryEvent{Content: "看看图片", Role: entity.RoleGuest}
		if tc.current {
			event.Images = images
		}
		if tc.quoted {
			event.ReplyContext = &entity.SensoryEvent{Images: images}
		}
		input, err := cortex.buildChatInput(context.Background(), event)
		if err != nil {
			t.Fatal(err)
		}
		var sources []string
		for _, definition := range toolsForEvent(hands, event, true, input.imageSources) {
			if definition.Function.Name == "analyze_images" {
				fields := definition.Function.Parameters["properties"].(map[string]any)
				sources = fields["source"].(map[string]any)["enum"].([]string)
			}
		}
		if !slices.Equal(sources, tc.want) {
			t.Fatalf("current=%t quoted=%t: sources=%v, want %v", tc.current, tc.quoted, sources, tc.want)
		}
	}
}
