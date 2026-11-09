package qq

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// QQ WebSocket Gateway 消息载荷结构
type wsPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"` // 序列号
	T  string          `json:"t,omitempty"` // 事件名
}

// StartWebSocket 启动并维护 WebSocket 长连接与自动重连
func (q *QQChannel) StartWebSocket(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if err := q.runWebSocketSession(ctx); err != nil && ctx.Err() == nil {
					slog.Error("qq websocket session disconnected, reconnecting in 5s...", "err", err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

// FetchGatewayURL 获取官方网关地址
func (c *Client) FetchGatewayURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/gateway/bot", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "QQBot "+c.Token())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("fetch gateway failed [status=%d]: %s", resp.StatusCode, string(body))
	}

	var res struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.URL, nil
}

func (q *QQChannel) runWebSocketSession(ctx context.Context) error {
	// 确保 AccessToken 已经就绪
	for i := 0; i < 10; i++ {
		if q.client.Token() != "" {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	if q.client.Token() == "" {
		return fmt.Errorf("access token not ready")
	}

	gatewayURL, err := q.client.FetchGatewayURL(ctx)
	if err != nil {
		return fmt.Errorf("failed to get gateway url: %w", err)
	}

	slog.Info("connecting to qq websocket gateway...", "url", gatewayURL)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, gatewayURL, nil)
	if err != nil {
		return fmt.Errorf("dial websocket error: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(2 * 1024 * 1024) // 2MB，防止富媒体大事件消息载荷触发 Gorilla 默认 32KB 限制断线

	slog.Info("connected to qq websocket gateway successfully")

	var (
		lastSeq         *int64
		seqMu           sync.Mutex
		heartbeatStop   = make(chan struct{})
		heartbeatOnce   sync.Once
		writeMu         sync.Mutex
		hbInterval      = 30 * time.Second
		isResumeAttempt = false
	)
	defer close(heartbeatStop)
	defer func() {
		if isResumeAttempt {
			q.clearWSSession()
		}
	}()

	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-heartbeatStop:
		}
	}()

	sendJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(v)
	}

	// 初始读取超时设置为 60s，等待 Hello 包
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// 消息读取循环
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read message error: %w", err)
		}

		// 成功收到任何数据包，刷新读取超时看门狗 (3倍心跳周期)
		_ = conn.SetReadDeadline(time.Now().Add(hbInterval * 3))

		var payload wsPayload
		if err := json.Unmarshal(message, &payload); err != nil {
			slog.Warn("failed to parse ws payload", "err", err)
			continue
		}

		if payload.S != nil {
			seqMu.Lock()
			lastSeq = payload.S
			seqMu.Unlock()
			q.setWSLastSeq(*payload.S)
		}

		switch payload.Op {
		case 10: // Hello 包，获取心跳周期并启动心跳与鉴权 (支持断线 Op 6 Resume 无损续接)
			var helloData struct {
				HeartbeatInterval int `json:"heartbeat_interval"`
			}
			_ = json.Unmarshal(payload.D, &helloData)
			interval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond
			if interval <= 0 {
				interval = 30 * time.Second
			}
			hbInterval = interval

			// 启动后台心跳协程 (Once 保护，防止多 Hello 包重复启动)
			heartbeatOnce.Do(func() {
				go func() {
					ticker := time.NewTicker(interval)
					defer ticker.Stop()
					for {
						select {
						case <-heartbeatStop:
							return
						case <-ticker.C:
							seqMu.Lock()
							s := lastSeq
							seqMu.Unlock()
							_ = sendJSON(map[string]any{
								"op": 1,
								"d":  s,
							})
						}
					}
				}()
			})

			prevSessionID, prevSeq := q.getWSSession()
			if prevSessionID != "" && prevSeq > 0 {
				isResumeAttempt = true
				seqCopy := prevSeq
				seqMu.Lock()
				lastSeq = &seqCopy
				seqMu.Unlock()
				resume := map[string]any{
					"op": 6,
					"d": map[string]any{
						"token":      "QQBot " + q.client.Token(),
						"session_id": prevSessionID,
						"seq":        prevSeq,
					},
				}
				if err := sendJSON(resume); err != nil {
					q.clearWSSession()
					return fmt.Errorf("send resume failed: %w", err)
				}
				slog.Info("sent resume (op 6) to qq gateway...", "sessionID", prevSessionID, "seq", prevSeq)
				continue
			}

			// 发送鉴权包订阅对应事件
			intents := (1 << 25) | (1 << 30)
			identify := map[string]any{
				"op": 2,
				"d": map[string]any{
					"token":   "QQBot " + q.client.Token(),
					"intents": intents,
					"shard":   []int{0, 1},
					"properties": map[string]any{
						"$os":      "linux",
						"$browser": "daidai_bot",
						"$device":  "daidai_bot",
					},
				},
			}
			if err := sendJSON(identify); err != nil {
				return fmt.Errorf("send identify failed: %w", err)
			}
			slog.Info("sent identify to qq gateway, awaiting ready...")

		case 11: // 服务端心跳 ACK 回应包
			slog.Debug("qq ws: 收到心跳 ACK (Op 11)")

		case 0: // Dispatch 事件分发
			isResumeAttempt = false
			if payload.T == "READY" {
				var readyData struct {
					SessionID string `json:"session_id"`
				}
				if err := json.Unmarshal(payload.D, &readyData); err == nil && readyData.SessionID != "" {
					q.setWSSessionID(readyData.SessionID)
				}
				slog.Info("QQ Bot WebSocket Gateway is READY, 机器人已全线连接上线", "sessionID", readyData.SessionID)
				continue
			}
			if payload.T == "RESUMED" {
				slog.Info("QQ Bot WebSocket session RESUMED, 断线期间事件已无损续接")
				continue
			}

			var rawData map[string]any
			if err := json.Unmarshal(payload.D, &rawData); err == nil {
				eventMap := map[string]any{
					"t": payload.T,
					"d": rawData,
				}
				q.dispatchEvent(eventMap)
			}

		case 1: // 服务端要求立即心跳
			seqMu.Lock()
			s := lastSeq
			seqMu.Unlock()
			_ = sendJSON(map[string]any{
				"op": 1,
				"d":  s,
			})

		case 7: // 服务端要求重连
			return fmt.Errorf("server requested reconnect (op 7)")
		case 9: // 鉴权失败或会话无效，清空旧 session 以便下次重连走 Identify
			q.clearWSSession()
			return fmt.Errorf("invalid session (op 9)")
		}
	}
}
