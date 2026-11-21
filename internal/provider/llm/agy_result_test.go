package llm

import "testing"

func TestParseAGYResultIgnoresProgressAfterAnswer(t *testing.T) {
	const progress = `{"type":"status","status":"RUNNING","conversation_id":"progress"}`
	const answer = `{"status":"SUCCESS","response":"正文","finished_reason":"max_tokens"}`
	raw := []byte(progress + "\nstatus log {details}\n" + answer + "\n" + progress)
	result, err := ParseAGYResult(raw)
	if err != nil || result.Response != "正文" || result.FinishedReason != "max_tokens" {
		t.Fatalf("unexpected result: %+v, %v", result, err)
	}
	if _, err := ParseAGYResult([]byte(progress)); err == nil {
		t.Fatal("progress-only output was accepted as an answer")
	}
}
