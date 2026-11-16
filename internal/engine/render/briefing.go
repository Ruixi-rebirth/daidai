package render

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed briefing.typ
var briefingTemplate string

type BriefingItem struct {
	Number       string `json:"number"`
	Page         int    `json:"page"`
	Source       string `json:"source"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	Meta         string `json:"meta"`
	EvidenceKind string `json:"evidence_kind"`
}
type BriefingDocument struct {
	Date       string         `json:"date"`
	Weekday    string         `json:"weekday"`
	Cutoff     string         `json:"cutoff"`
	Basic      bool           `json:"basic"`
	Sample     bool           `json:"sample"`
	SourceNote string         `json:"source_note"`
	Items      []BriefingItem `json:"items"`
	Leads      []string       `json:"leads"`
	PageNumber int            `json:"page_number"`
	TotalPages int            `json:"total_pages"`
}

func RenderBriefing(ctx context.Context, doc BriefingDocument) ([][]byte, error) {
	if len(doc.Items) == 0 || len(doc.Items) > 12 {
		return nil, fmt.Errorf("briefing requires 1–12 items")
	}
	for n, i := range doc.Items {
		if i.Page != n/3+1 {
			return nil, fmt.Errorf("invalid briefing page grouping")
		}
	}
	doc.TotalPages = doc.Items[len(doc.Items)-1].Page
	if doc.Leads == nil {
		doc.Leads = []string{}
	}
	var images [][]byte
	for p := 1; p <= doc.TotalPages; p++ {
		doc.PageNumber = p
		b, err := json.Marshal(doc)
		if err != nil {
			return nil, err
		}
		card, err := renderTypstAssets(ctx, briefingTemplate, map[string][]byte{"content.json": b})
		if err != nil {
			return nil, err
		}
		images = append(images, card)
	}
	return images, nil
}
