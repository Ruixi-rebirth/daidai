package chat

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"daidai/internal/core"
	"daidai/internal/db"
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
			if got := entity.ToolAllowed("manage_access", tc.event); got != tc.want {
				t.Fatalf("ToolAllowed() = %t, want %t", got, tc.want)
			}
		})
	}
	if !entity.ToolAllowed("manage_memory", &entity.SensoryEvent{Platform: "telegram", Role: entity.RoleGuest}) {
		t.Fatal("access filtering must not hide unrelated tools")
	}
	if !entity.ToolAllowed("manage_group_expressions", &entity.SensoryEvent{Platform: "telegram", ChannelID: "group", IsGroup: true, Role: entity.RolePartner}) {
		t.Fatal("group expression settings should be available to the partner in a group")
	}
	if entity.ToolAllowed("manage_group_expressions", &entity.SensoryEvent{Platform: "qq", Role: entity.RolePartner}) {
		t.Fatal("group expression settings should be hidden outside groups")
	}
	if !entity.ToolAllowed("manage_group_image_quota", &entity.SensoryEvent{Platform: "qq", ChannelID: "group", IsGroup: true, Role: entity.RolePartner}) {
		t.Fatal("group image quota settings should be available to the partner in a group")
	}
	if entity.ToolAllowed("manage_group_image_quota", &entity.SensoryEvent{Platform: "qq", ChannelID: "group", IsGroup: true, Role: entity.RoleGuest}) {
		t.Fatal("group image quota settings should be hidden from non-partners")
	}
	for _, role := range []entity.UserRole{entity.RolePartner, entity.RoleCollaborator, entity.RoleFriend, entity.RoleGuest} {
		if !entity.ToolAllowed("generate_image", &entity.SensoryEvent{Platform: "qq", Role: role}) {
			t.Fatalf("generate_image should be allowed for role %s", role)
		}
	}
	if entity.ToolAllowed("manage_geek_briefing", &entity.SensoryEvent{Role: entity.RoleCollaborator}) {
		t.Fatal("briefing management should be hidden from non-partners")
	}

	// Collaborator/Friend/Guest must NOT access infra, billing, system status, models, workspaces, or engineering tickets.
	partnerOnlyTools := []string{"oci_bill", "oci_ip", "singbox_links", "get_system_status", "switch_model", "switch_workspace", "delegate_ticket"}
	for _, toolName := range partnerOnlyTools {
		for _, role := range []entity.UserRole{entity.RoleCollaborator, entity.RoleFriend, entity.RoleGuest} {
			if entity.ToolAllowed(toolName, &entity.SensoryEvent{Platform: "qq", Role: role}) {
				t.Fatalf("%s must be hidden from role %s", toolName, role)
			}
		}
	}
	// Private-only infra, workspace, and engineering tools must stay hidden from Partner in groups.
	for _, toolName := range []string{"oci_bill", "oci_ip", "singbox_links", "manage_access", "switch_workspace", "delegate_ticket"} {
		if entity.ToolAllowed(toolName, &entity.SensoryEvent{Platform: "qq", ChannelID: "group", IsGroup: true, Role: entity.RolePartner}) {
			t.Fatalf("%s must remain private-only even for partner", toolName)
		}
		if !entity.ToolAllowed(toolName, &entity.SensoryEvent{Platform: "qq", Role: entity.RolePartner}) {
			t.Fatalf("%s should be available to partner in QQ private chat", toolName)
		}
	}
	if entity.ToolAllowed("unknown_tool", &entity.SensoryEvent{Platform: "qq", Role: entity.RolePartner}) {
		t.Fatal("unregistered tool must be denied by default")
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
		for _, definition := range toolsForEvent(hands, event, input.imageSources) {
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

func TestSystemPromptInjectsWorkspaceOnlyInPartnerPrivateChat(t *testing.T) {
	base := t.TempDir()
	store := db.NewManager(filepath.Join(base, "db"))
	t.Cleanup(func() { _ = store.Close() })
	sandbox := filepath.Join(base, "sandbox")
	workspaces := entity.NewWorkspaceManager(sandbox, store)
	cortex := NewCortex(CortexConfig{
		LLM:        &preparedViewModel{},
		Persona:    entity.NewDefaultPersona(),
		Workspaces: workspaces,
	})

	_, privateRuntime := cortex.buildSystemPrompt(&entity.SensoryEvent{
		Platform: "qq", ChannelID: "owner", Role: entity.RolePartner,
	}, chatRequestContext{})
	if !strings.Contains(privateRuntime, "【当前工作空间】") || !strings.Contains(privateRuntime, sandbox) {
		t.Fatalf("partner private prompt should include active workspace, got: %s", privateRuntime)
	}

	for _, ev := range []*entity.SensoryEvent{
		{Platform: "qq", ChannelID: "group", IsGroup: true, Role: entity.RolePartner},
		{Platform: "qq", ChannelID: "collab", Role: entity.RoleCollaborator},
		{Platform: "qq", ChannelID: "friend", Role: entity.RoleFriend},
		{Platform: "qq", ChannelID: "group", IsGroup: true, Role: entity.RoleGuest},
	} {
		sys, rt := cortex.buildSystemPrompt(ev, chatRequestContext{})
		if strings.Contains(rt, "【当前工作空间】") || strings.Contains(rt, sandbox) {
			t.Fatalf("non-partner-private event (%+v) leaked workspace in runtime context: %s", ev, rt)
		}
		if ev.Role != entity.RolePartner && strings.Contains(rt, "模型：") {
			t.Fatalf("non-partner event (%+v) should not receive internal model telemetry in runtime context: %s", ev, rt)
		}
		if ev.IsGroup && ev.Role == entity.RolePartner && !strings.Contains(sys, "私聊找你处理") {
			t.Fatalf("partner in group should receive clear private-chat redirect boundary instead of misleading permission toggle phrasing: %s", sys)
		}
	}
}
