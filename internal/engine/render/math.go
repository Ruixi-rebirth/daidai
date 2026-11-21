package render

import (
	"strings"
	"unicode"
)

// LaTeXToTypstMath converts standard LaTeX math expressions (as supported by KaTeX/MathJax)
// to Typst math syntax. If it encounters unrecognized constructs, it cleanly falls back
// to safe token representations so Typst can compile without error.
func LaTeXToTypstMath(latex string) string {
	s := strings.TrimSpace(latex)
	if s == "" {
		return ""
	}

	var sb strings.Builder
	runes := []rune(s)
	n := len(runes)
	i := 0

	for i < n {
		r := runes[i]

		// Whitespace
		if unicode.IsSpace(r) {
			sb.WriteRune(' ')
			for i < n && unicode.IsSpace(runes[i]) {
				i++
			}
			continue
		}

		// Comments in LaTeX (%)
		if r == '%' {
			for i < n && runes[i] != '\n' {
				i++
			}
			continue
		}

		// LaTeX macro starting with \
		if r == '\\' {
			i++
			if i >= n {
				break
			}
			// Single-char non-alpha macro like \%, \$, \&, \{, \}, \_, \, \; \: \! \\
			if !unicode.IsLetter(runes[i]) {
				ch := runes[i]
				i++
				switch ch {
				case '\\':
					sb.WriteString(" \\ ")
				case '{':
					sb.WriteString(" { ")
				case '}':
					sb.WriteString(" } ")
				case '%', '$', '&', '_', '#':
					sb.WriteRune(ch)
				case ',', ';', ':', ' ':
					sb.WriteString(" thin ")
				case '!':
					// negative thin space
					sb.WriteRune(' ')
				default:
					sb.WriteRune(ch)
				}
				continue
			}

			// Word macro: read letters
			start := i
			for i < n && unicode.IsLetter(runes[i]) {
				i++
			}
			macro := string(runes[start:i])

			// Check structural macros first
			switch macro {
			case "frac", "dfrac", "tfrac":
				num, nextI := readLatexArg(runes, i)
				i = nextI
				den, nextI := readLatexArg(runes, i)
				i = nextI
				sb.WriteString("frac(")
				sb.WriteString(LaTeXToTypstMath(num))
				sb.WriteString(", ")
				sb.WriteString(LaTeXToTypstMath(den))
				sb.WriteString(")")
				continue

			case "sqrt":
				// Optional degree argument [n]
				var deg string
				if i < n && runes[i] == '[' {
					closeBracket := -1
					for j := i + 1; j < n; j++ {
						if runes[j] == ']' {
							closeBracket = j
							break
						}
					}
					if closeBracket != -1 {
						deg = string(runes[i+1 : closeBracket])
						i = closeBracket + 1
					}
				}
				arg, nextI := readLatexArg(runes, i)
				i = nextI
				sb.WriteString("sqrt(")
				if deg != "" {
					sb.WriteString(LaTeXToTypstMath(deg))
					sb.WriteString(", ")
				}
				sb.WriteString(LaTeXToTypstMath(arg))
				sb.WriteString(")")
				continue

			case "text", "textrm", "textit", "textbf", "mathrm", "mathbf", "mathit", "operatorname":
				arg, nextI := readLatexArg(runes, i)
				i = nextI
				switch macro {
				case "mathbf":
					sb.WriteString("bold(")
					sb.WriteString(LaTeXToTypstMath(arg))
					sb.WriteString(")")
				case "mathit":
					sb.WriteString("italic(")
					sb.WriteString(LaTeXToTypstMath(arg))
					sb.WriteString(")")
				default:
					sb.WriteString(`"`)
					sb.WriteString(escapeTypstStringContent(arg))
					sb.WriteString(`"`)
				}
				continue

			case "left", "right":
				// In Typst, delimiters like () [] {} scale automatically, or lr() can be used.
				// Skip \left and \right prefixes and handle delimiter following
				for i < n && unicode.IsSpace(runes[i]) {
					i++
				}
				if i < n {
					delim := runes[i]
					i++
					if delim == '.' {
						// null delimiter in LaTeX, e.g. \right.
						continue
					}
					if delim == '\\' && i < n && (runes[i] == '{' || runes[i] == '}') {
						sb.WriteString(" \\" + string(runes[i]) + " ")
						i++
						continue
					}
					sb.WriteRune(' ')
					sb.WriteRune(delim)
					sb.WriteRune(' ')
				}
				continue

			case "begin":
				env, nextI := readLatexArg(runes, i)
				i = nextI
				i = handleEnvironment(runes, i, env, &sb)
				continue

			case "pm":
				sb.WriteString(" plus.minus ")
				continue
			case "mp":
				sb.WriteString(" minus.plus ")
				continue
			case "times":
				sb.WriteString(" times ")
				continue
			case "div":
				sb.WriteString(" div ")
				continue
			case "cdot":
				sb.WriteString(" dot ")
				continue
			case "le", "leq":
				sb.WriteString(" <= ")
				continue
			case "ge", "geq":
				sb.WriteString(" >= ")
				continue
			case "ne", "neq":
				sb.WriteString(" != ")
				continue
			case "approx":
				sb.WriteString(" approx ")
				continue
			case "equiv":
				sb.WriteString(" equiv ")
				continue
			case "sim":
				sb.WriteString(" tilde ")
				continue
			case "to", "rightarrow":
				sb.WriteString(" -> ")
				continue
			case "gets", "leftarrow":
				sb.WriteString(" <- ")
				continue
			case "leftrightarrow":
				sb.WriteString(" <-> ")
				continue
			case "Rightarrow":
				sb.WriteString(" => ")
				continue
			case "Leftarrow":
				sb.WriteString(" <= ")
				continue
			case "Leftrightarrow":
				sb.WriteString(" <=> ")
				continue
			case "mapsto":
				sb.WriteString(" |-> ")
				continue
			case "infty":
				sb.WriteString(" oo ")
				continue
			case "sum":
				sb.WriteString(" sum ")
				continue
			case "prod":
				sb.WriteString(" product ")
				continue
			case "coprod":
				sb.WriteString(" product.co ")
				continue
			case "int":
				sb.WriteString(" integral ")
				continue
			case "iint":
				sb.WriteString(" integral.double ")
				continue
			case "iiint":
				sb.WriteString(" integral.triple ")
				continue
			case "oint":
				sb.WriteString(" integral.cont ")
				continue
			case "partial":
				sb.WriteString(" diff ")
				continue
			case "nabla":
				sb.WriteString(" nabla ")
				continue
			case "forall":
				sb.WriteString(" forall ")
				continue
			case "exists":
				sb.WriteString(" exists ")
				continue
			case "nexists":
				sb.WriteString(" exists.not ")
				continue
			case "in":
				sb.WriteString(" in ")
				continue
			case "notin":
				sb.WriteString(" in.not ")
				continue
			case "ni":
				sb.WriteString(" in.rev ")
				continue
			case "subset":
				sb.WriteString(" subset ")
				continue
			case "supset":
				sb.WriteString(" supset ")
				continue
			case "subseteq":
				sb.WriteString(" subset.eq ")
				continue
			case "supseteq":
				sb.WriteString(" supset.eq ")
				continue
			case "cap":
				sb.WriteString(" sect ")
				continue
			case "cup":
				sb.WriteString(" union ")
				continue
			case "setminus":
				sb.WriteString(" without ")
				continue
			case "circ":
				sb.WriteString(" compose ")
				continue
			case "ldots", "cdots", "dots":
				sb.WriteString(" ... ")
				continue
			case "quad":
				sb.WriteString(" quad ")
				continue
			case "qquad":
				sb.WriteString(" quad quad ")
				continue
			case "star", "ast":
				sb.WriteString(" * ")
				continue
			case "hat":
				arg, nextI := readLatexArg(runes, i)
				i = nextI
				sb.WriteString("hat(")
				sb.WriteString(LaTeXToTypstMath(arg))
				sb.WriteString(")")
				continue
			case "bar", "overline":
				arg, nextI := readLatexArg(runes, i)
				i = nextI
				sb.WriteString("overline(")
				sb.WriteString(LaTeXToTypstMath(arg))
				sb.WriteString(")")
				continue
			case "vec":
				arg, nextI := readLatexArg(runes, i)
				i = nextI
				sb.WriteString("arrow(")
				sb.WriteString(LaTeXToTypstMath(arg))
				sb.WriteString(")")
				continue
			case "dot":
				arg, nextI := readLatexArg(runes, i)
				i = nextI
				sb.WriteString("dot(")
				sb.WriteString(LaTeXToTypstMath(arg))
				sb.WriteString(")")
				continue
			case "ddot":
				arg, nextI := readLatexArg(runes, i)
				i = nextI
				sb.WriteString("dot.double(")
				sb.WriteString(LaTeXToTypstMath(arg))
				sb.WriteString(")")
				continue
			case "tilde":
				arg, nextI := readLatexArg(runes, i)
				i = nextI
				sb.WriteString("tilde(")
				sb.WriteString(LaTeXToTypstMath(arg))
				sb.WriteString(")")
				continue
			}

			// Greek letters or known functions
			if isGreekOrKnownSymbol(macro) {
				sb.WriteRune(' ')
				sb.WriteString(typstSymbolForMacro(macro))
				sb.WriteRune(' ')
				continue
			}

			// Fallback: render macro name as safe text token
			sb.WriteString(` "`)
			sb.WriteString(escapeTypstStringContent(macro))
			sb.WriteString(`" `)
			continue
		}

		// Curly braces { } in LaTeX are for grouping, map to () or spaces unless escaped
		if r == '{' {
			arg, nextI := readLatexArg(runes, i-1) // include {
			i = nextI
			sb.WriteString(" (")
			sb.WriteString(LaTeXToTypstMath(arg))
			sb.WriteString(") ")
			continue
		}

		// In Typst math, multiple consecutive letters (e.g. "ac") are treated as an identifier.
		// If the user writes $2ac$ or $abc$, in LaTeX each letter is an individual variable.
		// Separate consecutive ASCII letters with spaces to keep standard math variable semantics.
		if unicode.IsLetter(r) && r < 128 {
			sb.WriteRune(' ')
			sb.WriteRune(r)
			sb.WriteRune(' ')
			i++
			continue
		}

		// Subscript / Superscript
		if r == '_' || r == '^' {
			sb.WriteRune(r)
			i++
			// Read following argument or char
			for i < n && unicode.IsSpace(runes[i]) {
				i++
			}
			if i < n && runes[i] == '{' {
				arg, nextI := readLatexArg(runes, i)
				i = nextI
				sb.WriteString("(")
				sb.WriteString(LaTeXToTypstMath(arg))
				sb.WriteString(")")
			} else if i < n {
				// single token
				sb.WriteString(LaTeXToTypstMath(string(runes[i])))
				i++
			}
			continue
		}

		// Default: pass through punctuation / digits / unicode
		sb.WriteRune(r)
		i++
	}

	return strings.TrimSpace(sb.String())
}

func readLatexArg(runes []rune, start int) (string, int) {
	n := len(runes)
	i := start
	for i < n && unicode.IsSpace(runes[i]) {
		i++
	}
	if i >= n {
		return "", n
	}

	if runes[i] == '{' {
		braceCount := 1
		i++
		contentStart := i
		for i < n {
			if runes[i] == '\\' && i+1 < n {
				i += 2
				continue
			}
			switch runes[i] {
			case '{':
				braceCount++
			case '}':
				braceCount--
				if braceCount == 0 {
					return string(runes[contentStart:i]), i + 1
				}
			}
			i++
		}
		return string(runes[contentStart:i]), i
	}

	// Single token argument (e.g. \frac 1 2)
	if runes[i] == '\\' {
		macroStart := i
		i++
		for i < n && unicode.IsLetter(runes[i]) {
			i++
		}
		return string(runes[macroStart:i]), i
	}

	val := string(runes[i])
	return val, i + 1
}

func handleEnvironment(runes []rune, start int, env string, sb *strings.Builder) int {
	n := len(runes)
	i := start
	endPattern := `\end{` + env + `}`
	subStr := string(runes[i:])
	endIdx := strings.Index(subStr, endPattern)
	var body string
	if endIdx == -1 {
		body = subStr
		i = n
	} else {
		body = subStr[:endIdx]
		i += endIdx + len(endPattern)
	}

	switch env {
	case "matrix", "pmatrix", "bmatrix", "Bmatrix", "vmatrix", "Vmatrix":
		delimOpen := ""
		switch env {
		case "pmatrix":
			delimOpen = `delim: "("`
		case "bmatrix":
			delimOpen = `delim: "["`
		case "Bmatrix":
			delimOpen = `delim: "{"`
		case "vmatrix":
			delimOpen = `delim: "|"`
		case "Vmatrix":
			delimOpen = `delim: "||"`
		}

		rows := strings.Split(body, `\\`)
		sb.WriteString("mat(")
		if delimOpen != "" {
			sb.WriteString(delimOpen + ", ")
		}
		for rIdx, row := range rows {
			cols := strings.Split(row, "&")
			for cIdx, col := range cols {
				sb.WriteString(LaTeXToTypstMath(col))
				if cIdx < len(cols)-1 {
					sb.WriteString(", ")
				}
			}
			if rIdx < len(rows)-1 {
				sb.WriteString("; ")
			}
		}
		sb.WriteString(")")

	case "cases":
		rows := strings.Split(body, `\\`)
		sb.WriteString("cases(")
		for rIdx, row := range rows {
			cols := strings.Split(row, "&")
			for cIdx, col := range cols {
				sb.WriteString(LaTeXToTypstMath(col))
				if cIdx < len(cols)-1 {
					sb.WriteString(", ")
				}
			}
			if rIdx < len(rows)-1 {
				sb.WriteString(", ")
			}
		}
		sb.WriteString(")")

	case "aligned", "align", "align*":
		// Typst uses & for alignment directly in equations
		rows := strings.Split(body, `\\`)
		for rIdx, row := range rows {
			cols := strings.Split(row, "&")
			for cIdx, col := range cols {
				sb.WriteString(LaTeXToTypstMath(col))
				if cIdx < len(cols)-1 {
					sb.WriteString(" & ")
				}
			}
			if rIdx < len(rows)-1 {
				sb.WriteString(" \\ ")
			}
		}

	default:
		// Unknown environment: convert body
		sb.WriteString(LaTeXToTypstMath(body))
	}

	return i
}

func isGreekOrKnownSymbol(s string) bool {
	switch s {
	case "alpha", "beta", "gamma", "delta", "epsilon", "varepsilon", "zeta", "eta",
		"theta", "vartheta", "iota", "kappa", "lambda", "mu", "nu", "xi",
		"pi", "varpi", "rho", "varrho", "sigma", "varsigma", "tau", "upsilon",
		"phi", "varphi", "chi", "psi", "omega",
		"Gamma", "Delta", "Theta", "Lambda", "Xi", "Pi", "Sigma", "Upsilon",
		"Phi", "Psi", "Omega",
		"sin", "cos", "tan", "cot", "sec", "csc",
		"arcsin", "arccos", "arctan",
		"sinh", "cosh", "tanh", "coth",
		"ln", "log", "exp", "lim", "max", "min", "sup", "inf", "det", "gcd":
		return true
	}
	return false
}

func typstSymbolForMacro(s string) string {
	switch s {
	case "varepsilon":
		return "epsilon.alt"
	case "vartheta":
		return "theta.alt"
	case "varpi":
		return "pi.alt"
	case "varrho":
		return "rho.alt"
	case "varsigma":
		return "sigma.alt"
	case "varphi":
		return "phi.alt"
	default:
		return s
	}
}

func escapeTypstStringContent(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
