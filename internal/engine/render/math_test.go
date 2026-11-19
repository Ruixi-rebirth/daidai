package render

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLaTeXToTypstMath(t *testing.T) {
	tests := []struct {
		name     string
		latex    string
		contains []string
	}{
		{
			name:     "quadratic formula",
			latex:    `x = \frac{-b \pm \sqrt{b^2 - 4ac}}{2a}`,
			contains: []string{"frac(", "plus.minus", "sqrt(", "b ^2"},
		},
		{
			name:     "summation and greek",
			latex:    `\sum_{i=1}^n x_i = \alpha + \beta`,
			contains: []string{"sum _(i =1)", "^n", "alpha", "beta"},
		},
		{
			name:     "matrix",
			latex:    `\begin{pmatrix} 1 & 2 \\ 3 & 4 \end{pmatrix}`,
			contains: []string{`mat(delim: "(",`, "1", "2", "3", "4)"},
		},
		{
			name:     "text in math",
			latex:    `f(x) = \text{const}`,
			contains: []string{`"const"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := LaTeXToTypstMath(tc.latex)
			for _, exp := range tc.contains {
				if !strings.Contains(got, exp) {
					t.Errorf("LaTeXToTypstMath(%q) = %q, missing %q", tc.latex, got, exp)
				}
			}
		})
	}
}

func TestTaylorFull(t *testing.T) {
	md := `一句话说，泰勒公式就是**用多项式去逼近一个复杂的平滑函数**。

因为像 $\sin(x)$、$e^x$、$\ln(x)$ 这些函数，计算机或笔算很难直接算出某个具体值，但多项式（加减乘除和乘方）算起来极其容易。泰勒公式就是给复杂函数找一个多项式的“替身”。

### 核心思想：怎么逼近？
如果你想在某一点 $x_0$ 附近模仿一个函数 $f(x)$：
1. **0 阶逼近**：先保证起点一致，即 $P(x_0) = f(x_0)$。这只是一条水平线。
2. **1 阶逼近**：再保证变化趋势（斜率）一致，加上一阶导数：$f'(x_0)(x - x_0)$。这就是切线。
3. **2 阶逼近**：再保证弯曲程度（凹凸性）一致，加上二阶导数：$\frac{f''(x_0)}{2!}(x - x_0)^2$。
4. **$n$ 阶逼近**：只要函数足够光滑，你继续对齐三阶导、四阶导……对齐的导数越多，在这个点附近就贴得越紧。

### 通用形式
设函数在 $x_0$ 处有 $n$ 阶连续导数，展开式就是：

$$f(x) = f(x_0) + f'(x_0)(x - x_0) + \frac{f''(x_0)}{2!}(x - x_0)^2 + \cdots + \frac{f^{(n)}(x_0)}{n!}(x - x_0)^n + R_n(x)$$

其中 $R_n(x)$ 是余项，也就是你用前 $n$ 项近似带来的误差（常用佩亚诺余项 $o((x-x_0)^n)$ 或拉格朗日余项表示）。

当展开中心选在原点 $x_0 = 0$ 时，就是大家最熟悉的**麦克劳林公式**。

### 最常见的几个麦克劳林展开（$x \to 0$）
- $e^x = 1 + x + \frac{x^2}{2!} + \frac{x^3}{3!} + \cdots$
- $\sin x = x - \frac{x^3}{3!} + \frac{x^5}{5!} - \cdots$
- $\cos x = 1 - \frac{x^2}{2!} + \frac{x^4}{4!} - \cdots$
- $\ln(1+x) = x - \frac{x^2}{2} + \frac{x^3}{3} - \cdots$

### 它有什么用？
1. **工程与计算**：计算器算三角函数或指数，底层通常就是用泰勒展开截断到足够精度的项数。
2. **求极限**：高数里算 $0/0$ 型极限时，等价无穷小其实就是泰勒公式展开到一阶（或更高阶）的特例。
3. **优化与机器学习**：一阶泰勒展开对应梯度下降法，二阶泰勒展开对应牛顿法。`

	content := []byte(md)
	typst := MarkdownToTypst(content)
	t.Logf("Generated Typst:\n%s\n", typst)

	expectedSnippets := []string{
		`$ f ( x ) =  f ( x _0)`,
		`$P ( x _0) =  f ( x _0)$`,
		`frac(f ''( x _0), 2!)`,
		`$e ^x = 1 +  x  + frac(x ^2, 2!)`,
		`$sin   x  =  x  - frac(x ^3, 3!)`,
		`$cos   x  = 1 - frac(x ^2, 2!)`,
		`$ln (1+ x ) =  x  - frac(x ^2, 2)`,
	}
	for _, exp := range expectedSnippets {
		if !strings.Contains(typst, exp) {
			t.Errorf("MarkdownToTypst output missing expected math snippet: %q", exp)
		}
	}

	forbiddenSnippets := []string{
		`\\frac`,
		`\\cdots`,
		`x_"#"0`,
	}
	for _, forb := range forbiddenSnippets {
		if strings.Contains(typst, forb) {
			t.Errorf("MarkdownToTypst output contains unparsed raw LaTeX/broken syntax: %q", forb)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	png, err := RenderCard(ctx, string(content))
	if err != nil {
		t.Fatalf("RenderCard failed: %v", err)
	}
	if len(png) < 100 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("rendered invalid PNG")
	}
	t.Logf("Rendered PNG bytes: %d", len(png))
}
