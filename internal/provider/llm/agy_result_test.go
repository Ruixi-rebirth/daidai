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

func TestParseAGYResultStreamJSONExtractsFinalStepAndInitConvID(t *testing.T) {
	stream := `{"event":"init","conversation_id":"conv-stream-1","init":{"model":"gemini-3.8-flash"}}
{"event":"step_update","step_update":{"conversation_id":"conv-stream-1","step_index":1,"state":"DONE","step_type":"agent_response","text_delta":"正在进入 Nix 开发环境跑测试…\n"}}
{"event":"step_update","step_update":{"conversation_id":"conv-stream-1","step_index":2,"state":"DONE","step_type":"tool","tool_name":"run_command"}}
{"event":"step_update","step_update":{"conversation_id":"conv-stream-1","step_index":3,"state":"ACTIVE","step_type":"agent_response","text_delta":"### 最终结论\n"}}
{"event":"step_update","step_update":{"conversation_id":"conv-stream-1","step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"全部 21 个包测试已通过。"}}
{"event":"result","result":{"conversation_id":"conv-stream-1","status":"SUCCESS","response":"正在进入 Nix 开发环境跑测试…\n### 最终结论\n全部 21 个包测试已通过。"}}`

	got, err := ParseAGYResult([]byte(stream))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ConversationID != "conv-stream-1" || got.Status != "SUCCESS" {
		t.Fatalf("unexpected stream result metadata: %+v", got)
	}
	if got.FinalStepResponse != "### 最终结论\n全部 21 个包测试已通过。" {
		t.Fatalf("expected clean final step without intermediate monologue, got %q", got.FinalStepResponse)
	}
}
