package mcp_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arhuman/mnemos/internal/chunk"
	"github.com/arhuman/mnemos/internal/ingest"
	"github.com/arhuman/mnemos/internal/storage"
)

// pendingEntryJSON mirrors one entry of the mnemos.journal_pending response.
type pendingEntryJSON struct {
	URI        string `json:"uri"`
	Collection string `json:"collection"`
	Title      string `json:"title"`
	CapturedAt string `json:"captured_at"`
	ModifiedAt string `json:"modified_at"`
}

// journalTree writes one captured journal entry and one kb decision into a tree
// root, ingests it, and returns the store and the root.
func journalTree(t *testing.T) (*sql.DB, string) {
	t.Helper()

	root := t.TempDir()
	mustWrite(t, root, "journal/2026-09-24/turn.md",
		"---\ntype: journal\ntitle: Why sqlite\ncollection: journal\ncaptured_at: \"2026-09-24T10:00:00Z\"\n---\n\n## Request\n\nWhy sqlite?\n")
	mustWrite(t, root, "adr/0001.md", "---\ntype: decision\n---\n\n# Use sqlite\n\nEmbedded.\n")

	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, storage.Migrate(db))

	_, err = ingest.New(db, slog.New(slog.NewTextHandler(io.Discard, nil))).Run(
		context.Background(), ingest.Options{
			Root:       root,
			Collection: "kb",
			Rules:      ingest.Rules{Include: []string{"**/*.md"}},
			Chunking:   chunk.Config{TargetTokens: 700, OverlapTokens: 80},
		})
	require.NoError(t, err)

	return db, root
}

func toolNames(t *testing.T, cs *mcpsdk.ClientSession) map[string]bool {
	t.Helper()
	tools, err := cs.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	require.NoError(t, err)
	names := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}

	return names
}

// TestJournalPendingToolListsEntries covers the read-only tool: advertised on a
// read-only server and returning the unreviewed entry, never the kb document.
func TestJournalPendingToolListsEntries(t *testing.T) {
	db, _ := journalTree(t)
	cs := connect(t, db)
	require.True(t, toolNames(t, cs)["mnemos.journal_pending"], "journal_pending is always advertised")

	var out struct {
		Entries []pendingEntryJSON `json:"entries"`
	}
	res := callTool(t, cs, "mnemos.journal_pending", make(map[string]any), &out)
	require.False(t, res.IsError)
	require.Len(t, out.Entries, 1)
	require.Equal(t, "journal/2026-09-24/turn.md", out.Entries[0].URI)
	require.Equal(t, "journal", out.Entries[0].Collection)
	require.Equal(t, "Why sqlite", out.Entries[0].Title)
	require.Equal(t, "2026-09-24T10:00:00Z", out.Entries[0].CapturedAt)
}

// TestJournalPendingToolEmptyIsArray pins the wire shape on an empty tier: an
// empty array, not null, so a client can iterate without a nil check.
func TestJournalPendingToolEmptyIsArray(t *testing.T) {
	cs := connect(t, emptyStore(t))

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "mnemos.journal_pending", Arguments: make(map[string]any)})
	require.NoError(t, err)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	require.True(t, ok)
	require.JSONEq(t, `{"entries":[]}`, tc.Text)
}

// TestJournalPromoteToolAbsentWhenWriteDisabled is least capability: a
// read-only server never advertises the stamp.
func TestJournalPromoteToolAbsentWhenWriteDisabled(t *testing.T) {
	db, root := journalTree(t)
	cs := connectWith(t, db, srvCfg{treeRoot: root})

	require.False(t, toolNames(t, cs)["mnemos.journal_promote"],
		"mnemos.journal_promote must not be advertised when allow_write is false")
}

// TestJournalPromoteToolStampsEntry covers the write path over the wire: the
// tool is advertised, stamps the file, and the entry leaves the pending list.
func TestJournalPromoteToolStampsEntry(t *testing.T) {
	db, root := journalTree(t)
	cs := connectWith(t, db, okfyConfig(root))
	require.True(t, toolNames(t, cs)["mnemos.journal_promote"])

	var out struct {
		EntryURI   string `json:"entry_uri"`
		PromotedTo string `json:"promoted_to"`
	}
	res := callTool(t, cs, "mnemos.journal_promote",
		map[string]any{"entry_uri": "journal/2026-09-24/turn.md", "promoted_to": "adr/0001.md"}, &out)
	require.False(t, res.IsError)
	require.Equal(t, "journal/2026-09-24/turn.md", out.EntryURI)
	require.Equal(t, "adr/0001.md", out.PromotedTo)

	b, err := os.ReadFile(filepath.Join(root, "journal", "2026-09-24", "turn.md"))
	require.NoError(t, err)
	require.Contains(t, string(b), "promoted_to: adr/0001.md\n")

	var pending struct {
		Entries []pendingEntryJSON `json:"entries"`
	}
	callTool(t, cs, "mnemos.journal_pending", make(map[string]any), &pending)
	require.Empty(t, pending.Entries)

	res = callTool(t, cs, "mnemos.journal_promote",
		map[string]any{"entry_uri": "journal/2026-09-24/turn.md", "promoted_to": "none"}, nil)
	require.True(t, res.IsError, "a second stamp is refused, never silently applied")
}
