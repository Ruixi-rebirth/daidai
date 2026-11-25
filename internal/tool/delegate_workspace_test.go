package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func TestResolveTicketWorkspacePreservesAllowlistAndAliases(t *testing.T) {
	base := t.TempDir()
	sandbox := filepath.Join(base, "sandbox")
	project := filepath.Join(base, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	store := db.NewManager(filepath.Join(base, "db"))
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	workspaces := entity.NewWorkspaceManager(sandbox, store)
	if _, err := workspaces.Switch("qq", "group", project); err != nil {
		t.Fatal(err)
	}
	tool := NewDelegateToolWithWorkspace(nil, entity.NewImmuneSystem("owner"), workspaces)

	for _, test := range []struct {
		name      string
		requested string
		want      string
	}{
		{name: "default current workspace", want: project},
		{name: "current alias", requested: "current", want: project},
		{name: "bookmark name", requested: "project", want: project},
		{name: "sandbox alias", requested: "default", want: sandbox},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := tool.resolveTicketWorkspace("qq", "group", test.requested)
			if err != nil || got != test.want {
				t.Fatalf("resolveTicketWorkspace(%q) = %q, %v; want %q", test.requested, got, err, test.want)
			}
		})
	}
	if _, err := tool.resolveTicketWorkspace("qq", "group", filepath.Join(base, "unmanaged")); err == nil || !strings.Contains(err.Error(), "未纳管") {
		t.Fatalf("unmanaged workspace was not rejected: %v", err)
	}
}

func TestResolveTicketWorkspaceRequiresManagerForExplicitPath(t *testing.T) {
	tool := NewDelegateToolWithWorkspace(nil, entity.NewImmuneSystem("owner"), nil)
	if got, err := tool.resolveTicketWorkspace("qq", "group", ""); err != nil || got != "" {
		t.Fatalf("empty workspace should keep worker default: %q, %v", got, err)
	}
	if _, err := tool.resolveTicketWorkspace("qq", "group", "/tmp/project"); err == nil || !strings.Contains(err.Error(), "无法校验目标工作区") {
		t.Fatalf("explicit path without manager was not rejected: %v", err)
	}
}

func TestDelegateToolRejectsGroupInvocationEvenForPartner(t *testing.T) {
	delegates := entity.NewDelegates(attachmentSelectionWorker{name: "agy"})
	tool := NewDelegateToolWithWorkspace(delegates, entity.NewImmuneSystem("owner"), nil)
	ctx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "group_1",
		IsGroup:   true,
		SenderID:  "owner",
		Role:      entity.RolePartner,
	})
	res, err := tool.Execute(ctx, json.RawMessage(`{"goal":"check repo"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || !strings.Contains(res.Text, "私聊") {
		t.Fatalf("expected group delegate_ticket to require private chat, got: %+v", res)
	}
}
