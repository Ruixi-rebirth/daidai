package llm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestParseProviderCooldownAndSafeRejection(t *testing.T) {
	if got := ParseProviderCooldown(errors.New("Individual quota reached. Resets in 2h34m16s.")); got != 2*time.Hour {
		t.Fatalf("expected 2h clamped cooldown, got %v", got)
	}
	if got := ParseProviderCooldown(errors.New("Resets in 36m56s.")); got != 36*time.Minute+56*time.Second {
		t.Fatalf("expected 36m56s cooldown, got %v", got)
	}
	if got := ParseProviderCooldown(errors.New("UNAVAILABLE (code 503): No capacity available")); got != 60*time.Second {
		t.Fatalf("expected 60s cooldown for 503 capacity error, got %v", got)
	}
	if got := ParseProviderCooldown(errors.New("generic command failure")); got != 0 {
		t.Fatalf("expected 0 cooldown for generic error, got %v", got)
	}
	if !isSafeAGYUpstreamRejection("RESOURCE_EXHAUSTED (code 429)") || !isSafeAGYUpstreamRejection("UNAVAILABLE (code 503)") {
		t.Fatal("expected 429/503 to be classified as safe upstream rejections")
	}
	if isSafeAGYUpstreamRejection("process timed out after tool execution") {
		t.Fatal("unexpected safe classification for generic timeout error")
	}
}

func TestCleanupEphemeralAGYConversation(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	targetID := "12345678-1234-4123-8123-1234567890ab"
	otherID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	base := filepath.Join(tmpHome, ".gemini", "antigravity-cli")

	createFixture := func(id string) []string {
		paths := []string{
			filepath.Join(base, "brain", id, "scratch", "a.txt"),
			filepath.Join(base, "conversations", id+".db"),
			filepath.Join(base, "conversations", id+".db-wal"),
			filepath.Join(base, "annotations", id+".pbtxt"),
			filepath.Join(base, "implicit", id+".pb"),
		}
		for _, p := range paths {
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return paths
	}

	targetPaths := createFixture(targetID)
	otherPaths := createFixture(otherID)

	// Path traversal or invalid ID must be ignored safely.
	CleanupEphemeralAGYConversation("../brain")
	CleanupEphemeralAGYConversation(targetID)

	if _, err := os.Stat(filepath.Join(base, "brain", targetID)); !os.IsNotExist(err) {
		t.Fatalf("expected ephemeral brain directory to be removed, err=%v", err)
	}
	for _, p := range targetPaths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected ephemeral state file %s to be removed", p)
		}
	}
	for _, p := range otherPaths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("unrelated conversation state %s was touched: %v", p, err)
		}
	}
}

func TestParseAGYResultCapturesDeniedActions(t *testing.T) {
	raw := []byte(`{"conversation_id":"0b8b5b7c-d846-43b3-8d77-6d69e3865f93","status":"SUCCESS","response":"","denied_actions":[{"action":"command","display_name":"RunCommand"}]}`)
	got, err := ParseAGYResult(raw)
	if err != nil {
		t.Fatalf("ParseAGYResult failed: %v", err)
	}
	if summary := got.DeniedActionSummary(); summary != "RunCommand" {
		t.Fatalf("expected DeniedActionSummary RunCommand, got %q", summary)
	}
}
