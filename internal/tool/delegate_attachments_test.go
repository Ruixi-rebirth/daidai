package tool

import (
	"context"
	"strings"
	"testing"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/media"
)

type attachmentSelectionWorker struct{ name string }

func (w attachmentSelectionWorker) Name() string      { return w.name }
func (attachmentSelectionWorker) Description() string { return "test worker" }
func (attachmentSelectionWorker) ExecuteTicket(context.Context, *entity.Ticket) (string, error) {
	return "", nil
}

func TestSelectTicketAttachmentsRequiresExplicitProcessingScope(t *testing.T) {
	delegates := entity.NewDelegates(attachmentSelectionWorker{name: "agy"})
	inbox, err := media.NewInbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool := NewDelegateToolWithInbox(delegates, entity.NewImmuneSystem("owner"), nil, inbox)
	file := &core.Attachment{Kind: core.AttachmentFile, URL: "telegram-file:1", Name: "report.txt"}
	event := &entity.SensoryEvent{Content: "总结这个文件", Files: []*core.Attachment{file}, PendingFiles: []*core.Attachment{file}}
	args := delegateArgs{IncludeCurrentAttachments: true, IncludePendingAttachments: true}

	selected, err := tool.selectTicketAttachments(event, &args)
	if err != nil || len(selected) != 1 || selected[0] != file || args.Worker != "agy" {
		t.Fatalf("attachment scope was not selected and deduplicated: selected=%+v worker=%q err=%v", selected, args.Worker, err)
	}

	args = delegateArgs{}
	selected, err = tool.selectTicketAttachments(event, &args)
	if err != nil || len(selected) != 0 || args.Worker != "" {
		t.Fatalf("attachments were selected without explicit scope: selected=%+v worker=%q err=%v", selected, args.Worker, err)
	}
}

func TestSelectTicketAttachmentsRejectsAmbiguousOrUnavailableWork(t *testing.T) {
	file := &core.Attachment{Kind: core.AttachmentFile, URL: "telegram-file:1", Name: "report.txt"}
	event := &entity.SensoryEvent{Files: []*core.Attachment{file}, Content: ""}
	delegates := entity.NewDelegates(attachmentSelectionWorker{name: "agy"})
	inbox, err := media.NewInbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool := NewDelegateToolWithInbox(delegates, entity.NewImmuneSystem("owner"), nil, inbox)
	args := delegateArgs{IncludeCurrentAttachments: true}
	if _, err := tool.selectTicketAttachments(event, &args); err == nil || !strings.Contains(err.Error(), "告诉我具体目标") {
		t.Fatalf("missing task description was not rejected: %v", err)
	}

	event.Content = "读取这个文件"
	args.Worker = "local"
	if _, err := tool.selectTicketAttachments(event, &args); err == nil || !strings.Contains(err.Error(), "只能交给 AGY") {
		t.Fatalf("unsupported worker was not rejected: %v", err)
	}

	args.Worker = ""
	tool.inbox = nil
	if _, err := tool.selectTicketAttachments(event, &args); err == nil || !strings.Contains(err.Error(), "暂不可用") {
		t.Fatalf("missing inbox was not rejected: %v", err)
	}
}
