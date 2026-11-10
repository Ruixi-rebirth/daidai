package entity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/db"
)

func TestWorkspaceManager_ActivationAndBookmarks(t *testing.T) {
	tempBase := t.TempDir()
	dbMgr := db.NewManager(filepath.Join(tempBase, "db"))
	defer dbMgr.Close()

	// 模拟创建两个真实目录和一个未激活的目录
	projA := filepath.Join(tempBase, "project-alpha")
	projB := filepath.Join(tempBase, "project-beta")
	projC := filepath.Join(tempBase, "unactivated-project")
	_ = os.MkdirAll(projA, 0755)
	_ = os.MkdirAll(projB, 0755)
	_ = os.MkdirAll(projC, 0755)

	wm := NewWorkspaceManager(projA, dbMgr)

	// 1. 验证默认获取与自动激活
	curr, name := wm.GetCurrent("chan_qq_1")
	if curr != projA || name != "project-alpha" {
		t.Fatalf("expected default to be projA, got curr: %s, name: %s", curr, name)
	}

	// 此时书签列表仅包含 project-alpha，绝对不会包含 unactivated-project
	_, bms := wm.ListBookmarked("chan_qq_1")
	if len(bms) != 1 || bms[0].Path != projA || !bms[0].IsCurrent {
		t.Fatalf("expected only projA in bookmarks, got: %+v", bms)
	}

	// 2. 激活并切入 project-beta
	switched, err := wm.Switch("chan_qq_1", projB)
	if err != nil {
		t.Fatalf("switch to projB failed: %v", err)
	}
	if switched != projB {
		t.Errorf("expected switched to %s, got %s", projB, switched)
	}

	// 此时书签列表包含 project-alpha 与 project-beta，依然不包含未激活的 projC
	currAfter, bmsAfter := wm.ListBookmarked("chan_qq_1")
	if currAfter != projB {
		t.Errorf("expected current to be projB, got %s", currAfter)
	}
	if len(bmsAfter) != 2 {
		t.Fatalf("expected 2 activated bookmarks, got %d", len(bmsAfter))
	}

	// 3. 验证通过项目名切回 project-alpha
	switchedBack, errBack := wm.Switch("chan_qq_1", "project-alpha")
	if errBack != nil {
		t.Fatalf("switch back by name failed: %v", errBack)
	}
	if switchedBack != projA {
		t.Errorf("expected switched back to %s, got %s", projA, switchedBack)
	}

	// 4. 验证注销/移除书签
	// 尝试注销当前激活的 projA 应被拒绝
	errRmCurr := wm.Remove("chan_qq_1", "project-alpha")
	if errRmCurr == nil {
		t.Fatalf("expected error when removing currently active workspace, got nil")
	}

	// 注销非激活的 project-beta 成功
	errRmB := wm.Remove("chan_qq_1", "project-beta")
	if errRmB != nil {
		t.Fatalf("remove project-beta bookmark failed: %v", errRmB)
	}

	_, bmsFinal := wm.ListBookmarked("chan_qq_1")
	if len(bmsFinal) != 1 || bmsFinal[0].Path != projA {
		t.Fatalf("expected only projA after removal, got: %+v", bmsFinal)
	}

	// 5. 验证非法目录拒绝
	_, errInvalid := wm.Switch("chan_qq_1", "nonexistent_dir_999")
	if errInvalid == nil || !strings.Contains(errInvalid.Error(), "不存在") {
		t.Errorf("expected error for nonexistent dir, got: %v", errInvalid)
	}
}

func TestWorkspaceManager_AutoPrunePhysicallyDeletedWorkspace(t *testing.T) {
	tempBase := t.TempDir()
	dbMgr := db.NewManager(filepath.Join(tempBase, "db"))
	defer dbMgr.Close()

	sandbox := filepath.Join(tempBase, "sandbox")
	projA := filepath.Join(tempBase, "project-ghost")
	_ = os.MkdirAll(sandbox, 0755)
	_ = os.MkdirAll(projA, 0755)

	wm := NewWorkspaceManager(sandbox, dbMgr)

	// 1. 纳管 project-ghost 并切入
	_, err := wm.Switch("chan_qq_1", projA)
	if err != nil {
		t.Fatalf("switch to projA failed: %v", err)
	}

	// 2. 模拟从物理磁盘上强行删除 project-ghost
	_ = os.RemoveAll(projA)

	// 3. 验证 GetCurrent 自动降级回沙盒并清理失效的 project-ghost
	curr, _ := wm.GetCurrent("chan_qq_1")
	if curr != sandbox {
		t.Errorf("expected fallback to sandbox, got: %s", curr)
	}

	// 4. 验证 ListBookmarked 自动自愈，不再列出已物理删除的幽灵目录
	_, bms := wm.ListBookmarked("chan_qq_1")
	for _, b := range bms {
		if strings.Contains(b.Path, "project-ghost") {
			t.Errorf("expected ghost project to be pruned, but found in bookmarks: %+v", b)
		}
	}
}

func TestResolveSandboxDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	if home != "" {
		home = filepath.Clean(home)
		if res := ResolveSandboxDir(""); res != filepath.Join(home, "sandbox") {
			t.Errorf("expected empty to resolve to home/sandbox, got: %s", res)
		}
		if res := ResolveSandboxDir("~"); res != filepath.Join(home, "sandbox") {
			t.Errorf("expected ~ to resolve to home/sandbox, got: %s", res)
		}
		if res := ResolveSandboxDir(home); res != filepath.Join(home, "sandbox") {
			t.Errorf("expected exact home to resolve to home/sandbox, got: %s", res)
		}
	}

	custom := filepath.Join(t.TempDir(), "my-custom-box")
	if res := ResolveSandboxDir(custom); res != custom {
		t.Errorf("expected custom dir to be preserved, got: %s", res)
	}
}

func TestCleanSandbox_SafeAndEffective(t *testing.T) {
	tempBase := t.TempDir()
	sandbox := filepath.Join(tempBase, "sandbox")
	_ = os.MkdirAll(sandbox, 0755)

	// 创建沙盒内文件与 .gitignore
	_ = os.WriteFile(filepath.Join(sandbox, ".gitignore"), []byte("*\n"), 0644)
	_ = os.WriteFile(filepath.Join(sandbox, "draft.py"), []byte("print('hello')\n"), 0644)
	_ = os.MkdirAll(filepath.Join(sandbox, "sub_temp"), 0755)

	wm := NewWorkspaceManager(sandbox, nil)

	// 1. 清理沙盒
	if err := wm.CleanSandbox(); err != nil {
		t.Fatalf("CleanSandbox failed: %v", err)
	}

	// 2. 验证临时文件已清理，.gitignore 被保留
	if _, err := os.Stat(filepath.Join(sandbox, "draft.py")); !os.IsNotExist(err) {
		t.Errorf("expected draft.py to be removed")
	}
	if _, err := os.Stat(filepath.Join(sandbox, "sub_temp")); !os.IsNotExist(err) {
		t.Errorf("expected sub_temp to be removed")
	}
	if _, err := os.Stat(filepath.Join(sandbox, ".gitignore")); err != nil {
		t.Errorf("expected .gitignore to be preserved")
	}

	// 3. 验证当沙盒被意外置为根或家目录时拒绝清空
	home, _ := os.UserHomeDir()
	if home != "" {
		wmBad := &WorkspaceManager{defaultDir: home}
		if err := wmBad.CleanSandbox(); err == nil {
			t.Errorf("expected error when cleaning home dir, got nil")
		}
	}
}
