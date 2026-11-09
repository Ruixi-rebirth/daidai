package memory

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

func isSeparatorRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsControl(r)
}

// ExtractSearchTerms 基于标准库 Unicode 字符分类提取检索词元（英文/数字按连续词切分，连续汉字自动拆解二元词组以保障无空格中文召回）
func ExtractSearchTerms(query string) []string {
	clauses := strings.FieldsFunc(strings.ToLower(query), isSeparatorRune)
	if len(clauses) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(clauses)*2)
	var terms []string
	addTerm := func(t string) {
		if t == "" {
			return
		}
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			terms = append(terms, t)
		}
	}

	for _, clause := range clauses {
		runes := []rune(clause)
		if len(runes) <= 2 {
			addTerm(clause)
			continue
		}

		// 将连续汉字与连续非汉字（拉丁字母/数字等）分段处理
		start := 0
		for start < len(runes) {
			isHan := unicode.Is(unicode.Han, runes[start])
			end := start + 1
			for end < len(runes) && unicode.Is(unicode.Han, runes[end]) == isHan {
				end++
			}
			seg := runes[start:end]
			if isHan {
				if len(seg) <= 2 {
					addTerm(string(seg))
				} else {
					for i := 0; i+1 < len(seg); i++ {
						addTerm(string(seg[i : i+2]))
					}
				}
			} else {
				addTerm(string(seg))
			}
			start = end
		}
	}
	return terms
}

// extractTokenFreqs 提取单字、二元词组及完整连续词元的词频分布与文档长度
func extractTokenFreqs(s string) (map[string]int, int) {
	var clean []rune
	for _, r := range s {
		if isSeparatorRune(r) {
			continue
		}
		clean = append(clean, unicode.ToLower(r))
	}
	if len(clean) == 0 {
		return nil, 0
	}
	freqs := make(map[string]int, len(clean)*2)
	for i, r := range clean {
		freqs[string(r)]++
		if i+1 < len(clean) {
			freqs[string(clean[i:i+2])]++
		}
	}
	for _, word := range strings.FieldsFunc(strings.ToLower(s), isSeparatorRune) {
		if utf8.RuneCountInString(word) > 2 {
			freqs[word] += 2
		}
	}
	return freqs, len(clean)
}

// LexicalOverlap 计算两个文本的词法重合系数（基于标准 Unicode 分类过滤标点空白，融合单字与二元词组）[0, 1]
func LexicalOverlap(a, b string) float32 {
	setA, _ := extractTokenFreqs(a)
	setB, _ := extractTokenFreqs(b)
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}

	intersect := 0
	for tok := range setB {
		if _, ok := setA[tok]; ok {
			intersect++
		}
	}

	minSet := len(setA)
	if len(setB) < minSet {
		minSet = len(setB)
	}
	if minSet == 0 {
		return 0
	}
	return float32(intersect) / float32(minSet)
}

// ScoreBM25 计算查询词对候选文档集合的 Okapi BM25 稀疏相关性得分
func ScoreBM25(query string, docs []string) []float32 {
	n := len(docs)
	if n == 0 {
		return nil
	}
	qFreqs, _ := extractTokenFreqs(query)
	if len(qFreqs) == 0 {
		return make([]float32, n)
	}

	docFreqs := make([]map[string]int, n)
	docLens := make([]int, n)
	df := make(map[string]int, len(qFreqs))
	totalLen := 0

	for i, doc := range docs {
		freqs, dLen := extractTokenFreqs(doc)
		docFreqs[i] = freqs
		docLens[i] = dLen
		totalLen += dLen
		for tok := range qFreqs {
			if freqs[tok] > 0 {
				df[tok]++
			}
		}
	}

	avgDL := float64(totalLen) / float64(n)
	if avgDL < 1 {
		avgDL = 1
	}

	const (
		k1 = 1.5
		b  = 0.75
	)

	scores := make([]float32, n)
	qLower := strings.ToLower(strings.TrimSpace(query))
	qTerms := ExtractSearchTerms(qLower)

	for i, freqs := range docs {
		if docLens[i] == 0 {
			continue
		}
		var sum float64
		dl := float64(docLens[i])
		for tok := range qFreqs {
			tf := float64(docFreqs[i][tok])
			if tf == 0 {
				continue
			}
			docCount := float64(df[tok])
			idf := math.Log(1.0 + (float64(n)-docCount+0.5)/(docCount+0.5))
			weight := 1.0
			if utf8.RuneCountInString(tok) >= 2 {
				weight = 1.8
			}
			tfNorm := (tf * (k1 + 1.0)) / (tf + k1*(1.0-b+b*(dl/avgDL)))
			sum += idf * tfNorm * weight
		}

		cLower := strings.ToLower(strings.TrimSpace(freqs))
		for _, term := range qTerms {
			if utf8.RuneCountInString(term) >= 2 && strings.Contains(cLower, term) {
				sum += 1.2
			}
		}
		scores[i] = float32(sum)
	}
	return scores
}

// CosineSimilarity 计算两个稠密向量的余弦相似度 [-1, 1]
func CosineSimilarity(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		va := float64(a[i])
		vb := float64(b[i])
		dot += va * vb
		normA += va * va
		normB += vb * vb
	}
	if normA <= 0 || normB <= 0 {
		return 0
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom <= 0 || math.IsNaN(denom) {
		return 0
	}
	sim := float32(dot / denom)
	if math.IsNaN(float64(sim)) || math.IsInf(float64(sim), 0) {
		return 0
	}
	if sim > 1.0 {
		return 1.0
	} else if sim < -1.0 {
		return -1.0
	}
	return sim
}

// FuseRRF 使用倒数秩融合 (Reciprocal Rank Fusion) 将稀疏 BM25 得分与稠密向量余弦相似度合并为统一相关性分值
func FuseRRF(bm25Scores, vecScores []float32, k float64) []float32 {
	n := len(bm25Scores)
	if n == 0 {
		return nil
	}
	if k <= 0 {
		k = 60.0
	}

	hasVec := false
	for _, v := range vecScores {
		if v > 0 {
			hasVec = true
			break
		}
	}
	if !hasVec {
		out := make([]float32, n)
		copy(out, bm25Scores)
		return out
	}

	bm25Order := make([]int, 0, n)
	for i, s := range bm25Scores {
		if s > 0 {
			bm25Order = append(bm25Order, i)
		}
	}
	sort.Slice(bm25Order, func(i, j int) bool {
		return bm25Scores[bm25Order[i]] > bm25Scores[bm25Order[j]]
	})

	vecOrder := make([]int, 0, n)
	for i := 0; i < n && i < len(vecScores); i++ {
		// 若已命中稀疏词元 (bm25 > 0)，允许 >= 0.35 的语义相似度参与重排；
		// 若零字面重合 (bm25 == 0)，则要求稠密向量余弦相似度 >= 0.48 方可跨词面召回，滤除预训练向量空间的基底噪声
		if vecScores[i] >= 0.48 || (bm25Scores[i] > 0 && vecScores[i] >= 0.35) {
			vecOrder = append(vecOrder, i)
		}
	}
	sort.Slice(vecOrder, func(i, j int) bool {
		return vecScores[vecOrder[i]] > vecScores[vecOrder[j]]
	})

	fused := make([]float32, n)
	for rank, idx := range bm25Order {
		rrf := float32(1.0 / (k + float64(rank+1)))
		fused[idx] += rrf*60.0 + bm25Scores[idx]*0.15
	}
	for rank, idx := range vecOrder {
		rrf := float32(1.0 / (k + float64(rank+1)))
		fused[idx] += rrf*60.0 + vecScores[idx]*0.5
	}
	return fused
}
