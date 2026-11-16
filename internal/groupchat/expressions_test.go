package groupchat

import (
	"context"
	"sync"
	"testing"
	"time"

	"daidai/internal/db"
)

func TestIndependentFlagsAtomicUpdatesAndGroupIsolation(t *testing.T) {
	database := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = database.Close() })
	store := &Store{Database: database}
	ctx := context.Background()
	state, err := store.Get(ctx, "qq", "group")
	if err != nil || state.Chatter || state.Dream {
		t.Fatal("expressions must default to disabled")
	}
	on := true
	var wg sync.WaitGroup
	for _, patch := range []Patch{{Chatter: &on}, {Dream: &on}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Update(ctx, "qq", "group", patch); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	state, err = store.Get(ctx, "qq", "group")
	if err != nil || !state.Chatter || !state.Dream {
		t.Fatal("concurrent setting change was lost")
	}
	for _, target := range [][2]string{{"qq", "other"}, {"telegram", "group"}, {"telegram", "group/7"}} {
		state, err = store.Get(ctx, target[0], target[1])
		if err != nil || state.Chatter || state.Dream {
			t.Fatal("settings leaked to another platform, group or topic")
		}
	}
	if err := store.Update(ctx, "qq", "group", Patch{}); err == nil {
		t.Fatal("empty update accepted")
	}
}

func TestSharedCooldownAndRarerDreamsPersist(t *testing.T) {
	database := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = database.Close() })
	store := &Store{Database: database}
	ctx := context.Background()
	on := true
	if err := store.Update(ctx, "telegram", "-100/7", Patch{Chatter: &on, Dream: &on}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.MarkSent(ctx, "telegram", "-100/7", Dream, now); err != nil {
		t.Fatal(err)
	}
	// A fresh store instance reads the same successful-send state after restart.
	state, err := (&Store{Database: database}).Get(ctx, "telegram", "-100/7")
	if err != nil {
		t.Fatal(err)
	}
	if chatter, dream := state.Available(now.Add(59 * time.Minute)); chatter || dream {
		t.Fatal("shared cooldown bypassed")
	}
	if chatter, dream := state.Available(now.Add(time.Hour)); !chatter || dream {
		t.Fatal("dream-specific cooldown was lost")
	}
	if chatter, dream := state.Available(now.Add(6 * time.Hour)); !chatter || !dream {
		t.Fatal("expired cooldown still blocked")
	}
	off := false
	if err = store.Update(ctx, "telegram", "-100/7", Patch{Dream: &off}); err != nil {
		t.Fatal(err)
	}
	state, err = store.Get(ctx, "telegram", "-100/7")
	if err != nil || state.LastDreamMS == 0 || state.Dream || !state.Chatter {
		t.Fatal("flag update erased cooldown or another setting")
	}
}
