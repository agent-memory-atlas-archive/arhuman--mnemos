package cli_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const cliJournalEntry = "---\ntype: journal\ntitle: Why sqlite\ncollection: journal\ncaptured_at: \"2026-09-24T10:00:00Z\"\n---\n\n## Request\n\nWhy sqlite?\n"

// seedJournalWorkspace initializes a workspace holding one captured journal
// entry and one kb decision, both indexed.
func seedJournalWorkspace(t *testing.T) {
	t.Helper()
	chdir(t, t.TempDir())
	runCmd(t, "init")
	seedKB(t, "journal/2026-09-24/turn.md", cliJournalEntry)
	seedKB(t, "adr/0001.md", "---\ntype: decision\n---\n\n# Use sqlite\n\nEmbedded.\n")
	runCmd(t, "ingest", ".", "--collection", "kb")
}

// TestJournalPendingCLIListsEntries covers the tabular and JSON forms, and that
// the kb document is never listed.
func TestJournalPendingCLIListsEntries(t *testing.T) {
	seedJournalWorkspace(t)

	out := runCmd(t, "journal", "pending")
	require.Contains(t, out, "journal/2026-09-24/turn.md")
	require.Contains(t, out, "2026-09-24T10:00:00Z")
	require.Contains(t, out, "Why sqlite")
	require.NotContains(t, out, "adr/0001.md")

	var entries []struct {
		URI        string `json:"uri"`
		CapturedAt string `json:"captured_at"`
		Title      string `json:"title"`
	}
	require.NoError(t, json.Unmarshal([]byte(runCmd(t, "journal", "pending", "--json")), &entries))
	require.Len(t, entries, 1)
	require.Equal(t, "journal/2026-09-24/turn.md", entries[0].URI)
	require.Equal(t, "Why sqlite", entries[0].Title)
}

// TestJournalPendingCLIEmpty prints the plain nothing-pending message, and an
// empty JSON array rather than null.
func TestJournalPendingCLIEmpty(t *testing.T) {
	chdir(t, t.TempDir())
	runCmd(t, "init")

	require.Equal(t, "no pending journal entries\n", runCmd(t, "journal", "pending"))
	require.Equal(t, "[]", strings.TrimSpace(runCmd(t, "journal", "pending", "--json")))
}

// TestJournalPromoteCLIStampsEntry covers the success path: the entry is
// stamped on disk and leaves the pending list.
func TestJournalPromoteCLIStampsEntry(t *testing.T) {
	seedJournalWorkspace(t)
	enableWrite(t)

	out := runCmd(t, "journal", "promote", "journal/2026-09-24/turn.md", "adr/0001.md")
	require.Equal(t, "journal/2026-09-24/turn.md promoted_to adr/0001.md\n", out)

	b, err := os.ReadFile(kbPath("journal/2026-09-24/turn.md"))
	require.NoError(t, err)
	require.Contains(t, string(b), "promoted_to: adr/0001.md\n")
	require.Equal(t, "no pending journal entries\n", runCmd(t, "journal", "pending"))
}

// TestJournalPromoteCLIRefusals pins that a refused promotion is a non-zero
// exit carrying the reason, unlike the hooks which swallow failures.
func TestJournalPromoteCLIRefusals(t *testing.T) {
	seedJournalWorkspace(t)

	_, err := runCmdErr(t, "journal", "promote", "journal/2026-09-24/turn.md", "none")
	require.Error(t, err)
	require.Contains(t, err.Error(), "allow_write")

	enableWrite(t)
	_, err = runCmdErr(t, "journal", "promote", "journal/2026-09-24/turn.md", "adr/missing.md")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown uri")

	_, err = runCmdErr(t, "journal", "promote", "adr/0001.md", "none")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a journal entry")
}
