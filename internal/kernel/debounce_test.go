package kernel

import (
	"sync/atomic"
	"testing"
	"time"

	"daidai/internal/core"
)

func TestDebounceManager_MergeAndImmediateControlBypass(t *testing.T) {
	var callCount int32
	var finalMergedText string

	done := make(chan struct{})
	ctrlDone := make(chan string, 1)

	handler := func(ev *core.SensoryEvent) {
		if ev.Content == "/stop" {
			ctrlDone <- ev.Content
			return
		}
		atomic.AddInt32(&callCount, 1)
		finalMergedText = ev.Content
		close(done)
	}

	mgr := NewDebounceManager(DebounceConfig{
		MergeWindow: 100 * time.Millisecond,
		IdleTimeout: 1 * time.Second,
	}, handler)
	defer mgr.Stop()

	// 模拟同一个用户在短时间内连发三句话，中间穿插 /stop 控制指令
	mgr.Push(&core.SensoryEvent{IsGroup: true, ChannelID: "90001", SenderID: "10001", Content: "第一句"})
	time.Sleep(20 * time.Millisecond)
	mgr.Push(&core.SensoryEvent{IsGroup: true, ChannelID: "90001", SenderID: "10001", Content: "/stop"})
	time.Sleep(20 * time.Millisecond)
	mgr.Push(&core.SensoryEvent{IsGroup: true, ChannelID: "90001", SenderID: "10001", Content: "第二句"})
	time.Sleep(20 * time.Millisecond)
	mgr.Push(&core.SensoryEvent{IsGroup: true, ChannelID: "90001", SenderID: "10001", Content: "第三句"})

	// 验证 /stop 不受防抖窗口阻塞且不被合并进普通对话
	select {
	case cmd := <-ctrlDone:
		if cmd != "/stop" {
			t.Fatalf("expected immediate /stop, got %q", cmd)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("控制面指令 /stop 未能直通触发")
	}

	// 等待防抖触发
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("防抖合并超时未触发")
	}

	if atomic.LoadInt32(&callCount) != 1 {
		t.Fatalf("预期只触发 1 次调用，实际触发了 %d 次", callCount)
	}

	expected := "第一句\n第二句\n第三句"
	if finalMergedText != expected {
		t.Fatalf("合并文本不匹配，得到: %q, 预期: %q", finalMergedText, expected)
	}
}
