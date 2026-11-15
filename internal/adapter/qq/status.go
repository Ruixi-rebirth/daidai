package qq

import (
	"fmt"
	"time"
)

type sendObservation struct {
	at      time.Time
	success bool
}

// FormatChannelStatus reports observations, rather than claiming that one
// successful destination establishes proactive access for all recipients.
func (q *QQChannel) FormatChannelStatus() string {
	state := "未连接"
	if q.Ready() {
		state = "已连接"
	}
	return fmt.Sprintf("- QQ 收消息网关（WebSocket）：%s\n- QQ 主动私聊：%s\n- QQ 主动群消息：%s", state, formatSendObservation(q.proactivePrivate.Load()), formatSendObservation(q.proactiveGroup.Load()))
}

func formatSendObservation(observation *sendObservation) string {
	if observation == nil {
		return "本次启动尚无发送记录"
	}
	result := "最近一次发送成功"
	if !observation.success {
		result = "最近一次发送未完整完成"
	}
	return result + "（" + observation.at.Format("01-02 15:04:05 MST") + "）"
}
