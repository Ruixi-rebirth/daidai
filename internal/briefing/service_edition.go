package briefing

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/engine/render"
)

func (s *Service) generate(parent context.Context) (*Edition, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	now := s.Now()
	id := now.Format("2006-01-02")
	if e, _, err := s.Store.Edition(ctx, id); err != nil || e != nil {
		return e, err
	}
	if err := s.Store.Begin(ctx, id, now); err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			save, cancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			defer cancel()
			if err := s.Store.Fail(save, id, "generation_failed"); err != nil {
				slog.Error("briefing: 保存失败状态失败", "edition", id, "err", err)
			}
		}
	}()
	collectCtx, collectCancel := context.WithTimeout(ctx, 60*time.Second)
	defer collectCancel()
	items, states, cutoff, err := s.Collector.Collect(collectCtx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	seen, err := s.Store.RecentIdentities(ctx, now)
	if err != nil {
		return nil, err
	}
	items = candidates(items, seen)
	items = parallelItems(collectCtx, items, s.Collector.readme)
	for index := range items {
		items[index].Evidence = clipped(items[index].Evidence, 1400)
	}
	selected, leads, basic := edit(ctx, s.Model, items)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	localCutoff := cutoff.In(s.loc)
	if localCutoff.Format("2006-01-02") != id {
		return nil, fmt.Errorf("briefing collection crossed local midnight")
	}
	e := &Edition{ID: id, Date: now.Format("2006.01.02"), Weekday: []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[now.Weekday()], Cutoff: localCutoff.Format("15:04 MST"), CutoffAt: cutoff, Timezone: s.loc.String(), SourceStates: states, Basic: basic}
	if s.Model != nil {
		e.Model = s.Model.Name()
	}
	finalize(e, selected, leads)
	if err = s.Store.Save(ctx, e); err != nil {
		return nil, err
	}
	complete = true
	slog.Info("briefing: 已归档早报", "edition", e.ID, "items", len(e.Items), "basic", e.Basic)
	return e, nil
}
func (s *Service) Present(ctx context.Context, e *Edition) ([][]byte, []string, error) {
	select {
	case s.renderSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-s.renderSlot }()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	doc := render.BriefingDocument{Date: e.Date, Weekday: e.Weekday, Cutoff: e.Cutoff, Basic: e.Basic, Leads: e.Leads}
	var notes []string
	for _, st := range e.SourceStates {
		if st.Status == "error" || st.Status == "partial" {
			notes = append(notes, labels[st.Source]+"资料不完整")
		}
	}
	doc.SourceNote = strings.Join(notes, "；")
	for _, i := range e.OrderedItems() {
		doc.Items = append(doc.Items, render.BriefingItem{Number: i.Number, Source: i.Source, Title: i.Title, Summary: i.Summary, Meta: i.Meta, EvidenceKind: i.EvidenceKind})
	}
	if len(e.Items) == 0 {
		return nil, nil, nil
	}
	image, err := render.RenderBriefing(ctx, doc)
	if err != nil {
		return nil, nil, err
	}
	return [][]byte{image}, []string{e.Text()}, nil
}
