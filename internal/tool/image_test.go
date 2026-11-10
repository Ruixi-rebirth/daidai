package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

type dummyWorker struct{}

func (d *dummyWorker) Name() string                                              { return "dummy" }
func (d *dummyWorker) Description() string                                       { return "dummy worker" }
func (d *dummyWorker) ExecuteTicket(ctx context.Context, t *entity.Ticket) (string, error) {
	return "ok", nil
}

func TestImageTool_NotLoggedIn_FailFast(t *testing.T) {
	// 隔离 HOME 目录，模拟未登录状态
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// 确认当前未登录
	if err := llm.CheckAGYAuth(); !errors.Is(err, llm.ErrAGYNotLoggedIn) {
		t.Fatalf("expected ErrAGYNotLoggedIn, got %v", err)
	}

	delegates := entity.NewDelegates(&dummyWorker{})
	imgTool := NewImageTool(delegates)

	rawArgs, _ := json.Marshal(map[string]any{
		"prompt": "画一只小狗",
	})

	res, err := imgTool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("expected no tool runtime crash, got err: %v", err)
	}

	if !strings.Contains(res.Text, "尚未登录") || !strings.Contains(res.Text, "/agy login") {
		t.Fatalf("expected fail-fast prompt about login, got: %s", res.Text)
	}
}
