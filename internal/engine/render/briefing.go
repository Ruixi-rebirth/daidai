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
}

func RenderBriefing(ctx context.Context, doc BriefingDocument) ([]byte, error) {
	if len(doc.Items) == 0 || len(doc.Items) > 25 {
		return nil, fmt.Errorf("briefing requires 1–25 items")
	}
	if doc.Leads == nil {
		doc.Leads = []string{}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return renderTypstAssets(ctx, briefingTemplate, map[string][]byte{"content.json": b}, false)
}
