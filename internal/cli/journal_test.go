package cli_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

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
	require.Contains(t, err.Error(), "write the kb document first, or pass none")

	_, err = runCmdErr(t, "journal", "promote", "adr/0001.md", "none")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a journal entry")
	require.Contains(t, err.Error(), "mnemos journal pending")
}

// seedPruneWorkspace indexes an old reviewed entry alone in its dated dir, an
// old unreviewed entry and a reviewed entry captured now, returning the
// reviewed old entry's uri.
func seedPruneWorkspace(t *testing.T) string {
	t.Helper()
	chdir(t, t.TempDir())
	runCmd(t, "init")
	entry := func(captured, extra string) string {
		return "---\ntype: journal\ntitle: Turn\ncollection: journal\ncaptured_at: \"" + captured + "\"\n" + extra + "---\n\nBody.\n"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	seedKB(t, "journal/2020-01-01/old.md", entry("2020-01-01T10:00:00Z", "promoted_to: none\n"))
	seedKB(t, "journal/2020-01-02/open.md", entry("2020-01-02T10:00:00Z", ""))
	seedKB(t, "journal/today/young.md", entry(now, "promoted_to: none\n"))
	runCmd(t, "ingest", ".", "--collection", "kb")

	return "journal/2020-01-01/old.md"
}

// TestJournalPruneCLIDryRunAndJSON covers the default dry run in both forms:
// it names the old reviewed entry, counts the unreviewed one, deletes nothing.
func TestJournalPruneCLIDryRunAndJSON(t *testing.T) {
	old := seedPruneWorkspace(t)

	out := runCmd(t, "journal", "prune", "--older-than", "12h")
	require.Contains(t, out, "would prune 1 entries\n")
	require.Contains(t, out, old)
	require.Contains(t, out, "2020-01-01T10:00:00Z")
	require.Contains(t, out, "skipped: 1 unreviewed, 0 undated\n")
	require.NotContains(t, out, "young.md")

	var res struct {
		Pruned            []map[string]any `json:"pruned"`
		SkippedUnreviewed int              `json:"skipped_unreviewed"`
		RemovedDirs       []string         `json:"removed_dirs"`
		Applied           bool             `json:"applied"`
	}
	require.NoError(t, json.Unmarshal([]byte(runCmd(t, "journal", "prune", "--older-than", "30d", "--json")), &res))
	require.Len(t, res.Pruned, 1)
	require.Equal(t, old, res.Pruned[0]["uri"])
	require.Equal(t, 1, res.SkippedUnreviewed)
	require.False(t, res.Applied)
	require.NotNil(t, res.RemovedDirs)

	_, err := os.Stat(kbPath(old))
	require.NoError(t, err, "a dry run deletes nothing")
}

// TestJournalPruneCLIApplyRemoves covers --apply with a day-suffixed duration:
// the entry and its emptied dated dir are gone, the others stay.
func TestJournalPruneCLIApplyRemoves(t *testing.T) {
	old := seedPruneWorkspace(t)
	enableDelete(t)

	out := runCmd(t, "journal", "prune", "--older-than", "30d", "--apply")
	require.Contains(t, out, "pruned 1 entries\n")
	require.Contains(t, out, "removed dir journal/2020-01-01\n")

	_, err := os.Stat(kbPath("journal/2020-01-01"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(kbPath("journal/2020-01-02/open.md"))
	require.NoError(t, err)
	_, err = os.Stat(kbPath("journal/today/young.md"))
	require.NoError(t, err)
	require.NotContains(t, runCmd(t, "journal", "prune", "--older-than", "30d"), old)
}

// TestJournalPruneCLIRefusals pins the non-zero exits: a missing or malformed
// --older-than, a non-positive one, and --apply without allow_delete.
func TestJournalPruneCLIRefusals(t *testing.T) {
	old := seedPruneWorkspace(t)

	cases := []struct {
		args []string
		msg  string
	}{
		{[]string{"journal", "prune"}, "older-than"},
		{[]string{"journal", "prune", "--older-than", "soon"}, "invalid --older-than"},
		{[]string{"journal", "prune", "--older-than", "0d"}, "must be positive"},
		{[]string{"journal", "prune", "--older-than", "30d", "--apply"}, "allow_delete"},
	}
	for _, tc := range cases {
		_, err := runCmdErr(t, tc.args...)
		require.Error(t, err, strings.Join(tc.args, " "))
		require.Contains(t, err.Error(), tc.msg)
	}

	_, err := os.Stat(kbPath(old))
	require.NoError(t, err)
}
