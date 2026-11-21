package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"daidai/internal/entity"
)

func (c *Channel) call(ctx context.Context, method string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+c.cfg.Token+"/"+method, body)
	if err != nil {
		return fmt.Errorf("telegram request construction failed")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("telegram %s transport failed", method)
	}
	defer func() { _ = resp.Body.Close() }()
	var envelope struct {
		OK         bool            `json:"ok"`
		Result     json.RawMessage `json:"result"`
		Code       int             `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("telegram %s response could not be decoded", method)
	}
	if !envelope.OK {
		err := &apiError{method: method, code: envelope.Code, status: resp.StatusCode}
		if envelope.Code >= 400 && envelope.Code < 500 {
			delay := time.Duration(0)
			if envelope.Code == 429 {
				delay = max(time.Duration(envelope.Parameters.RetryAfter)*time.Second, time.Minute)
			}
			return &entity.DeliveryRejection{Err: err, After: delay}
		}
		return err
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}
func (c *Channel) jsonCall(ctx context.Context, method string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.call(ctx, method, bytes.NewReader(b), "application/json", out)
}

type apiError struct {
	method       string
	code, status int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram %s rejected (status %d, code %d)", e.method, e.status, e.code)
}

// VerifyOwnerPrivate resolves the existing address and rechecks the configured
// username. The numeric address is never an authorization binding.
