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

func TestMarkdownToTypst_TableInlineFormatting(t *testing.T) {
	md := "| 阶段 | 说明 |\n| :--- | :---: |\n| **预训练** | 学习 `通用表示` 与 $E=mc^2$ |\n| **微调 (SFT)** | 遵循 [指令](https://example.com) |\n"
	res := MarkdownToTypst([]byte(md))
	if !strings.Contains(res, "#table(columns: 2") {
		t.Fatalf("expected #table with 2 columns, got:\n%s", res)
	}
	if !strings.Contains(res, `#strong[#"预训练"]`) {
		t.Fatalf("expected bold text compiled to #strong, got:\n%s", res)
	}
	if !strings.Contains(res, `#raw(block: false, "通用表示")`) {
		t.Fatalf("expected inline code compiled to #raw, got:\n%s", res)
	}
	if !strings.Contains(res, `#mi("E=mc^2")`) {
		t.Fatalf("expected math compiled to Typst math, got:\n%s", res)
	}
	if !strings.Contains(res, `#link("https://example.com")[#"指令"]`) {
		t.Fatalf("expected link compiled to #link, got:\n%s", res)
	}
	if strings.Contains(res, `#"**预训练**"`) {
		t.Fatalf("asterisks leaked into typst string literal: %s", res)
	}
}

func TestMarkdownToTypst_Strikethrough(t *testing.T) {
	md := "这是 ~~已废弃内容~~ 文本"
	res := MarkdownToTypst([]byte(md))
	if !strings.Contains(res, `#strike[#"已废弃内容"]`) {
		t.Fatalf("expected strikethrough compiled to #strike, got:\n%s", res)
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

func TestRenderCard_Math(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	md := "# 数学公式测试\n\n行内公式测试：质能方程为 $E = mc^2$，求根公式为 $x = \\frac{-b \\pm \\sqrt{b^2 - 4ac}}{2a}$，价格 $100 原样展示。\n\n块级公式测试：\n$$\n\\sum_{i=1}^n i = \\frac{n(n+1)}{2}\n$$\n\n矩阵测试：\n$$\n\\begin{pmatrix} 1 & 2 \\\\ 3 & 4 \\end{pmatrix}\n$$\n"
	pngBytes, err := RenderCard(ctx, md)
	if err != nil {
		t.Fatalf("RenderCard with LaTeX math failed: %v", err)
	}

	if len(pngBytes) < 100 {
		t.Fatalf("rendered png is suspiciously small: %d bytes", len(pngBytes))
	}
	if string(pngBytes[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("expected PNG header magic, got: %v", pngBytes[:8])
	}
}

func TestShouldRenderCard(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected bool
	}{
		{"empty string", "", false},
		{"short casual chat", "好的，收到啦！马上帮你办。", false},
		{"moderate chat below threshold", strings.Repeat("字", 60), false},
		{"plain text over 80 runes", strings.Repeat("字", 85), true},
		{"3 lines over 60 runes", strings.Repeat("这是一整行较长且充实的内容讨论与分析说明。\n", 3), true},
		{"short 2 lines under 60 runes", "第一行\n第二行", false},
		{"structured list over 4 lines", "- 方案 A：快速接入\n- 方案 B：稳定隔离\n- 方案 C：混合架构\n最后一条备注", true},
		{"heading with 60+ runes", "# 模块状态概述\n" + strings.Repeat("系统运行平稳，各项指标正常。", 5), true},
		{"short formula under 20 runes", "质能方程是 $E = mc^2$。", true},
		{"block formula under 30 runes", "$$\\sum_{i=1}^n i$$", true},
		{"currency amount should not trigger card", "这个商品价格是 $100 块钱。", false},
		{"formula inside code block should not trigger card", "```\n$foo = 100$\n```", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual := ShouldRenderCard(tc.input)
			if actual != tc.expected {
				t.Errorf("ShouldRenderCard(%q) = %v; want %v", tc.name, actual, tc.expected)
			}
		})
	}
}

func TestMarkdownToTypst_CJKEmphasis(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantSub  string
		forbid   string
	}{
		{
			name:    "cjk before bold inline code",
			input:   "你现在这套大概率走的是**`AGY`**通道",
			wantSub: `#strong[#raw(block: false, "AGY")]`,
			forbid:  `#"**"`,
		},
		{
			name:    "cjk before bold cjk quotes",
			input:   "你现在这套大概率走的是**「主链路」**方案",
			wantSub: `#strong[#"「主链路」"]`,
			forbid:  `#"**"`,
		},
		{
			name:    "cjk before bold curly quotes",
			input:   "你现在这套大概率走的是**“直连模式”**的逻辑",
			wantSub: `#strong[#"“直连模式”"]`,
			forbid:  `#"**"`,
		},
		{
			name:    "cjk paren followed by cjk",
			input:   "**中文（补充说明）**的内容",
			wantSub: `#strong[#"中文（补充说明）"]`,
			forbid:  `#"**"`,
		},
		{
			name:    "literal multiplication preserved",
			input:   "计算 a * b 的值，以及 user_id 不变",
			wantSub: `user_`,
			forbid:  `#emph[`,
		},
		{
			name:    "inline code asterisks preserved",
			input:   "代码 `**not bold**` 原样保留",
			wantSub: `#raw(block: false, "**not bold**")`,
			forbid:  `#strong[`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MarkdownToTypst([]byte(tc.input))
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("expected %q in output, got:\n%s", tc.wantSub, got)
			}
			if tc.forbid != "" && strings.Contains(got, tc.forbid) {
				t.Fatalf("forbidden %q leaked into output:\n%s", tc.forbid, got)
			}
		})
	}
}
