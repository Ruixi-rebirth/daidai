package qq

import (
	"context"
	"strings"
	"testing"

	"daidai/internal/entity"
)

func TestProactiveStatusTracksOnlyItsDeliveryScope(t *testing.T) {
	q := &QQChannel{}
	if err := q.Transmit(context.Background(), &entity.Utterance{Proactive: true, Text: "report"}); err == nil {
		t.Fatal("missing client unexpectedly sent message")
	}
	if q.proactivePrivate.Load() == nil || q.proactivePrivate.Load().success {
		t.Fatal("failed proactive send not recorded")
	}
	if q.proactiveGroup.Load() != nil {
		t.Fatal("private send changed group observation")
	}
	previous := q.proactivePrivate.Load()
	_ = q.Transmit(context.Background(), &entity.Utterance{Text: "passive reply"})
	if q.proactivePrivate.Load() != previous {
		t.Fatal("passive delivery changed proactive observation")
	}
	status := q.FormatChannelStatus()
	if !strings.Contains(status, "未完整完成") || !strings.Contains(status, "本次启动尚无发送记录") {
		t.Fatalf("misleading status: %s", status)
	}
}
