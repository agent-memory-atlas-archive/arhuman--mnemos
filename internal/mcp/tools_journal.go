package mcp

import (
	"context"
	"errors"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arhuman/mnemos/internal/memory"
)

// journalPendingInput is the mnemos.journal_pending request.
type journalPendingInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum number of entries, oldest first (0 = all)"`
}

// journalPendingOutput is the mnemos.journal_pending response: the journal
// entries not yet reviewed, oldest first.
type journalPendingOutput struct {
	Entries []memory.JournalEntry `json:"entries"`
}

// journalPromoteInput is the mnemos.journal_promote request.
type journalPromoteInput struct {
	EntryURI   string `json:"entry_uri"   jsonschema:"the uri of the journal entry that was reviewed"`
	PromotedTo string `json:"promoted_to" jsonschema:"the uri of the kb document now holding the entry's content (already written with mnemos.remember), or none when nothing was worth promoting"`
}

// registerJournalPending wires the read-only mnemos.journal_pending tool. It is
// always registered and needs no allow_* gate: listing the journal is not a
// write (ADR-0011: the journal is excluded from search, never from read).
func (s *Server) registerJournalPending() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "mnemos.journal_pending",
		Description: "List session journal entries not yet reviewed (no promoted_to), oldest first. Journal entries are excluded from search; read one with mnemos.read, then record the outcome with mnemos.journal_promote.",
	}, s.handleJournalPending)
}

func (s *Server) handleJournalPending(ctx context.Context, _ *mcpsdk.CallToolRequest, in journalPendingInput) (*mcpsdk.CallToolResult, any, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	entries, err := s.svc.ListPendingJournal(ctx, in.Limit)
	if err != nil {
		return nil, nil, err
	}

	return s.result(journalPendingOutput{Entries: entries})
}

// registerJournalPromote wires mnemos.journal_promote. It is called only when
// [mcp].allow_write is true, so a disabled write surface is never advertised in
// tools/list (least capability).
func (s *Server) registerJournalPromote() {
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "mnemos.journal_promote",
		Description: "Record that a journal entry was handled, by stamping its promoted_to field. Call it AFTER the kb document has been written with mnemos.remember, passing that document's uri; pass none when the entry was reviewed and nothing was worth promoting. This tool never writes the kb document itself. An entry already stamped is refused.",
	}, s.handleJournalPromote)
}

func (s *Server) handleJournalPromote(ctx context.Context, _ *mcpsdk.CallToolRequest, in journalPromoteInput) (*mcpsdk.CallToolResult, any, error) {
	// Defensive re-check: the tool is only registered when allow_write is true,
	// and the service re-checks too, but the handler refuses regardless.
	if !s.cfg.MCP.AllowWrite {
		return nil, nil, errors.New("write disabled: set [mcp].allow_write=true")
	}

	res, err := s.svc.PromoteJournal(ctx, memory.PromoteJournalInput{EntryURI: in.EntryURI, Target: in.PromotedTo})
	if err != nil {
		return nil, nil, err
	}

	return s.result(res)
}
