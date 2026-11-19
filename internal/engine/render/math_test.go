package render

import (
	"strings"
	"testing"
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
