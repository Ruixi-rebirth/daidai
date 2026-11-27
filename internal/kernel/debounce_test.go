package kernel

import (
	"sync"
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

func TestTargetedStopUsesImmediateControlChannel(t *testing.T) {
	if !isImmediateControlEvent("/stop tkt_123") {
		t.Fatal("targeted stop would wait behind ordinary turns")
	}
	if isImmediateControlEvent("/stopping tkt_123") {
		t.Fatal("ordinary text was treated as a stop command")
	}
}

func TestMergedTurnPreservesEarliestHistoryBoundary(t *testing.T) {
	merged := mergeEvents([]*core.SensoryEvent{
		{HistoryRecordIDs: []int64{10}, Content: "第一条"},
		{HistoryRecordIDs: []int64{11, 12}, Content: "第二批"},
		{HistoryRecordIDs: []int64{12, 14, 12, 0}, Content: "重叠批次"},
	})
	if merged.LastHistoryRecordID() != 12 || merged.HistoryBoundaryID() != 10 || len(merged.HistoryRecordIDs) != 6 || merged.HistoryRecordIDs[1] != 11 || merged.HistoryRecordIDs[2] != 12 {
		t.Fatalf("history boundaries lost: %+v", merged)
	}
}

func TestMergedTurnPreservesRoleAndInputMetadata(t *testing.T) {
	latest := &core.SensoryEvent{
		Platform: "qq", ChannelID: "private", SenderID: "friend",
		Role:          core.RoleFriend,
		OmittedImages: 2, StillValid: func() bool { return false },
	}
	merged := mergeEvents([]*core.SensoryEvent{
		{Content: "第一句", Role: core.RoleFriend, OmittedImages: 1}, latest,
	})
	if merged.Role != core.RoleFriend || merged.StillValid == nil || merged.StillValid() {
		t.Fatal("merged message lost its role or validity metadata")
	}
	if merged.OmittedImages != 3 || latest.OmittedImages != 2 {
		t.Fatalf("incorrect aggregate or mutated input: merged=%d latest=%d", merged.OmittedImages, latest.OmittedImages)
	}
}

func TestDebounceManager_GroupMembersRunConcurrently(t *testing.T) {
	aliceStarted := make(chan struct{})
	releaseAlice := make(chan struct{})
	aliceDone := make(chan struct{})
	bobDone := make(chan struct{})
	carolDone := make(chan struct{})

	mgr := NewDebounceManager(DebounceConfig{
		MergeWindow: 20 * time.Millisecond,
		IdleTimeout: 1 * time.Second,
	}, func(ev *core.SensoryEvent) {
		switch ev.SenderID {
		case "alice":
			close(aliceStarted)
			<-releaseAlice
			close(aliceDone)
		case "bob":
			close(bobDone)
		case "carol":
			close(carolDone)
		}
	})
	defer mgr.Stop()

	mgr.Push(&core.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "g1", SenderID: "alice", Content: "draw"})
	select {
	case <-aliceStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("alice did not start")
	}

	mgr.Push(&core.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "g1", SenderID: "bob", Content: "hi"})
	mgr.Push(&core.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "g1", SenderID: "carol", Content: "casual", GroupMessageObserved: true})

	select {
	case <-bobDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("bob was blocked by alice in the same group")
	}
	select {
	case <-carolDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("carol observed message was blocked by alice in the same group")
	}

	close(releaseAlice)
	select {
	case <-aliceDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("alice did not finish after release")
	}
}

func TestDebounceManager_GroupStopOnlyCancelsSenderAndResetBarrierAppliesToAll(t *testing.T) {
	t.Run("StopOnlyCancelsSender", func(t *testing.T) {
		var aliceDelivered atomic.Bool
		stopDelivered := make(chan struct{})
		bobDelivered := make(chan struct{})

		mgr := NewDebounceManager(DebounceConfig{
			MergeWindow: 80 * time.Millisecond,
			IdleTimeout: 1 * time.Second,
		}, func(ev *core.SensoryEvent) {
			switch {
			case ev.Content == "/stop":
				close(stopDelivered)
			case ev.SenderID == "alice":
				aliceDelivered.Store(true)
			case ev.SenderID == "bob":
				close(bobDelivered)
			}
		})
		defer mgr.Stop()

		mgr.Push(&core.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "g1", SenderID: "alice", Role: core.RolePartner, Content: "alice msg"})
		mgr.Push(&core.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "g1", SenderID: "bob", Role: core.RolePartner, Content: "bob msg"})
		time.Sleep(15 * time.Millisecond)
		mgr.Push(&core.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "g1", SenderID: "alice", Role: core.RolePartner, Content: "/stop"})

		select {
		case <-stopDelivered:
		case <-time.After(300 * time.Millisecond):
			t.Fatal("/stop did not fire")
		}
		select {
		case <-bobDelivered:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("bob's pending turn was wrongly cancelled by alice's /stop")
		}
		time.Sleep(120 * time.Millisecond)
		if aliceDelivered.Load() {
			t.Fatal("alice's pending turn was not cancelled by /stop")
		}
	})

	t.Run("ResetBarrierAppliesToAllMembers", func(t *testing.T) {
		resetStarted := make(chan struct{})
		releaseReset := make(chan struct{})
		bobDelivered := make(chan struct{})

		mgr := NewDebounceManager(DebounceConfig{
			MergeWindow: 20 * time.Millisecond,
			IdleTimeout: 1 * time.Second,
		}, func(ev *core.SensoryEvent) {
			if ev.Content == "/reset" {
				close(resetStarted)
				<-releaseReset
				return
			}
			if ev.SenderID == "bob" {
				close(bobDelivered)
			}
		})
		defer mgr.Stop()

		mgr.Push(&core.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "g1", SenderID: "alice", Role: core.RolePartner, Content: "/reset"})
		select {
		case <-resetStarted:
		case <-time.After(300 * time.Millisecond):
			t.Fatal("/reset did not start")
		}

		mgr.Push(&core.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "g1", SenderID: "bob", Content: "after reset"})
		select {
		case <-bobDelivered:
			t.Fatal("bob's group turn ran before /reset barrier finished")
		case <-time.After(80 * time.Millisecond):
		}

		close(releaseReset)
		select {
		case <-bobDelivered:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("bob's group turn did not resume after /reset barrier released")
		}
	})
}

func TestDebounceManager_CoalescesQueuedTurnsWhileBusy(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var received []*core.SensoryEvent
	done := make(chan struct{}, 4)

	mgr := NewDebounceManager(DebounceConfig{
		MergeWindow: 20 * time.Millisecond,
		IdleTimeout: 1 * time.Second,
	}, func(ev *core.SensoryEvent) {
		mu.Lock()
		received = append(received, ev)
		count := len(received)
		mu.Unlock()
		if count == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		done <- struct{}{}
	})
	defer mgr.Stop()

	// Turn 1 starts and blocks handler.
	mgr.Push(&core.SensoryEvent{Platform: "qq", ChannelID: "u1", SenderID: "u1", RawMessageID: "m1", Content: "第一条"})
	select {
	case <-firstStarted:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("first turn did not start")
	}

	// Turn 2 and Turn 3 arrive outside each other's merge window while Turn 1 is busy.
	mgr.Push(&core.SensoryEvent{Platform: "qq", ChannelID: "u1", SenderID: "u1", RawMessageID: "m2", Content: "对了顺便查A"})
	time.Sleep(50 * time.Millisecond)
	mgr.Push(&core.SensoryEvent{Platform: "qq", ChannelID: "u1", SenderID: "u1", RawMessageID: "m3", Content: "哦不对，查B就行"})
	time.Sleep(50 * time.Millisecond)
	// Turn 4 quotes a specific message -> must NOT merge with Turn 2+3.
	mgr.Push(&core.SensoryEvent{
		Platform:     "qq",
		ChannelID:    "u1",
		SenderID:     "u1",
		RawMessageID: "m4",
		Content:      "另外看看这条",
		ReplyContext: &core.SensoryEvent{RawMessageID: "old-1", Content: "旧消息"},
	})
	time.Sleep(50 * time.Millisecond)

	close(releaseFirst)

	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("expected 3 total handler invocations, timed out after %d", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 3 {
		t.Fatalf("expected 3 handler calls (m1, coalesced m2+m3, quoted m4), got %d", len(received))
	}
	if received[1].Content != "对了顺便查A\n哦不对，查B就行" || received[1].RawMessageID != "m3" {
		t.Fatalf("unexpected coalesced turn: content=%q msgID=%q", received[1].Content, received[1].RawMessageID)
	}
	if received[2].Content != "另外看看这条" || received[2].RawMessageID != "m4" {
		t.Fatalf("unexpected deferred quoted turn: content=%q msgID=%q", received[2].Content, received[2].RawMessageID)
	}
}
