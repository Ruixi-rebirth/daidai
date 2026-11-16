package tool

import (
	"context"
	"testing"

	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

func TestGroupExpressionManagementRequiresPartnerAndCurrentGroup(t *testing.T) {
	database := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = database.Close() })
	store := &groupchat.Store{Database: database}
	tool := NewGroupExpressionTool(store)
	for _, event := range []*entity.SensoryEvent{
		nil,
		{Platform: "qq", ChannelID: "owner", Role: entity.RolePartner},
		{Platform: "telegram", ChannelID: "-100", IsGroup: true, Role: entity.RoleCollaborator},
		{Platform: "qq", ChannelID: "group", IsGroup: true, Role: entity.RoleGuest},
	} {
		if _, err := tool.Execute(entity.WithSensoryEvent(context.Background(), event), []byte(`{"action":"set","chatter":true,"dream":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	state, err := store.Get(context.Background(), "qq", "group")
	if err != nil || state.Chatter || state.Dream {
		t.Fatal("unauthorized setting changed")
	}
	owner := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Platform: "qq", ChannelID: "group", IsGroup: true, Role: entity.RolePartner})
	if _, err = tool.Execute(owner, []byte(`{"action":"set","chatter":true,"dream":true,"group_id":"other-group"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = tool.Execute(owner, []byte(`{"action":"set","dream":false}`)); err != nil {
		t.Fatal(err)
	}
	state, err = store.Get(owner, "qq", "group")
	if err != nil || !state.Chatter || state.Dream {
		t.Fatal("independent mode update failed")
	}
	other, err := store.Get(owner, "qq", "other-group")
	if err != nil || other.Chatter || other.Dream {
		t.Fatal("request targeted another group")
	}
}
