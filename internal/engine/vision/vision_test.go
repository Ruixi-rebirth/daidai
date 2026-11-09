package vision

import (
	"os/exec"
	"testing"
)

func TestVision_IsAvailableWithGenericCLI(t *testing.T) {
	eng := NewEngine(Config{})
	_, errAgy := exec.LookPath("agy")
	expectAvailable := errAgy == nil

	if eng.IsAvailable() != expectAvailable {
		t.Fatalf("expected IsAvailable to be %v, got %v", expectAvailable, eng.IsAvailable())
	}

	// 验证可显式指定自定义 CLI 命令
	customEng := NewEngine(Config{
		CLICommand: "nonexistent_custom_vision_cli",
	})
	if customEng.IsAvailable() {
		t.Fatalf("expected nonexistent CLI to be unavailable")
	}
}
