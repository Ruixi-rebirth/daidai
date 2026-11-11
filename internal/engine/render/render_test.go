package render

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMarkdownToTypst_AST(t *testing.T) {
	md := "# 一级标题\n## 二级标题\n\n这是一段普通文本，带有 **粗体文本** 和 _斜体文本_。\n这里还有符号 $100 与 @user 与 #tag 与 user_id，通过原生字符串字面量安全包裹。\n\n行内代码 `fmt.Println(\"$Hello\")` 中的符号原样保留。\n\n```go\npackage main\n\nfunc main() {\n    println(\"Hello from code block $100 #tag\")\n}\n```\n\n> 这是引用内容\n> 引用多行\n\n- 无序列表项 1\n- 无序列表项 2\n\n1. 有序列表项 1\n2. 有序列表项 2\n\n[链接测试](https://github.com)\n"
	res := MarkdownToTypst([]byte(md))

	if !strings.Contains(res, `= #"一级标题"`) {
		t.Fatalf("expected '= #\"一级标题\"', got:\n%s", res)
	}
	if !strings.Contains(res, `== #"二级标题"`) {
		t.Fatalf("expected '== #\"二级标题\"', got:\n%s", res)
	}
	if !strings.Contains(res, `#strong[#"粗体文本"]`) {
		t.Fatalf("expected '#strong[#\"粗体文本\"]', got:\n%s", res)
	}
	if !strings.Contains(res, `#emph[#"斜体文本"]`) {
		t.Fatalf("expected '#emph[#\"斜体文本\"]', got:\n%s", res)
	}
	if !strings.Contains(res, `#raw(block: false, "fmt.Println(\"$Hello\")")`) {
		t.Fatalf("expected #raw inline code, got:\n%s", res)
	}
	if !strings.Contains(res, `#raw(block: true, lang: "go"`) {
		t.Fatalf("expected #raw fenced code block, got:\n%s", res)
	}
	if !strings.Contains(res, "#block(stroke:") {
		t.Fatalf("expected blockquote container, got:\n%s", res)
	}
	if !strings.Contains(res, `#link("https://github.com")[#"链接测试"]`) {
		t.Fatalf("expected typst link, got:\n%s", res)
	}

	// 验证紧凑嵌套列表不会粘连在同一行，且具备层级缩进
	nestedMD := "- 父级列表项\n  - 子级列表项 A\n  - 子级列表项 B\n"
	nestedRes := MarkdownToTypst([]byte(nestedMD))
	if !strings.Contains(nestedRes, "- #\"父级列表项\"\n  - #\"子级列表项 A\"") {
		t.Fatalf("expected nested list newline and indentation, got:\n%s", nestedRes)
	}

	// 不允许 Markdown 图片引用服务器本地文件
	tmpImg := filepath.Join(t.TempDir(), "sample.png")
	_ = os.WriteFile(tmpImg, []byte("fake"), 0644)
	imgRes := MarkdownToTypst([]byte("![示意图](" + tmpImg + ")"))
	if strings.Contains(imgRes, "#image(") || !strings.Contains(imgRes, `#"[图片: 示意图]"`) {
		t.Fatalf("expected local image reference to render as a placeholder, got: %s", imgRes)
	}
}

func TestRenderCard_Compile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	md := "# 呆呆测试\n这是一张纯通过 AST 解析和 Typst 编译的高清卡片，支持任意特殊符号如 $100、#tag、@user、user_id、a * b。\n\n| 模块 | 状态 |\n| --- | --- |\n| 认知皮层 | 正常 |\n| 记忆中枢 | 正常 |\n\n```json\n{\"status\": \"ok\", \"user_id\": \"10001\"}\n```\n"
	pngBytes, err := RenderCard(ctx, md)
	if err != nil {
		t.Fatalf("RenderCard failed: %v", err)
	}

	if len(pngBytes) < 100 {
		t.Fatalf("rendered png is suspiciously small: %d bytes", len(pngBytes))
	}

	// 校验 PNG 文件头魔数: \x89PNG\r\n\x1a\n
	if string(pngBytes[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("expected PNG header magic, got: %v", pngBytes[:8])
	}
}
