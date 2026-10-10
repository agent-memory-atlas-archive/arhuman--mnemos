package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arhuman/mnemos/internal/memory"
)

// journalTurnAt builds a turn for session "abcdef12-3456" captured at a fixed
// instant, with the given outcome.
func journalTurnAt(outcome string) memory.JournalTurn {
	return memory.JournalTurn{
		SessionID: "abcdef12-3456",
		Request:   "do the thing\nwith details",
		At:        time.Date(2026, 10, 10, 5, 31, 42, 0, time.UTC),
		Files:     []string{"internal/cli/hook.go"},
		Outcome:   outcome,
	}
}

func readEntry(t *testing.T, f fixture, uri string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.treeRoot, filepath.FromSlash(uri)))
	require.NoError(t, err)

	return string(raw)
}

// TestWriteJournalEntryCreatesThenUpdates covers the contract: the path scheme,
// the frontmatter, and a second write for the same turn updating in place.
func TestWriteJournalEntryCreatesThenUpdates(t *testing.T) {
	f := newFixture(t, true, false)
	ctx := context.Background()

	res, err := f.svc.WriteJournalEntry(ctx, journalTurnAt("first outcome"))
	require.NoError(t, err)
	require.Equal(t, memory.JournalWriteResult{URI: "journal/2026-10-10/abcdef12-053142.md", Written: true}, res)
	body := readEntry(t, f, res.URI)
	require.True(t, strings.HasPrefix(body, "---\ntype: journal\ntitle: do the thing\ncollection: journal\n"), body)
	require.Contains(t, body, "captured_at: \"2026-10-10T05:31:42Z\"\nsession_id: abcdef12-3456\n")
	require.Contains(t, body, "## Files touched\n\n- internal/cli/hook.go\n")

	res, err = f.svc.WriteJournalEntry(ctx, journalTurnAt("second outcome"))
	require.NoError(t, err)
	require.True(t, res.Written)
	body = readEntry(t, f, res.URI)
	require.Contains(t, body, "## Outcome\n\nsecond outcome\n")
	require.NotContains(t, body, "first outcome")
	require.Equal(t, []string{res.URI}, pendingURIs(t, f.svc))
}

// TestWriteJournalEntryKeepsPromotedEntry is the P19 regression: a write for a
// turn whose entry was already reviewed leaves it untouched and out of pending.
func TestWriteJournalEntryKeepsPromotedEntry(t *testing.T) {
	f := newFixture(t, true, false)
	ctx := context.Background()
	res, err := f.svc.WriteJournalEntry(ctx, journalTurnAt("first outcome"))
	require.NoError(t, err)
	_, err = f.svc.PromoteJournal(ctx, memory.PromoteJournalInput{EntryURI: res.URI, Target: memory.PromoteNone})
	require.NoError(t, err)
	before := readEntry(t, f, res.URI)

	res, err = f.svc.WriteJournalEntry(ctx, journalTurnAt("second outcome"))
	require.NoError(t, err)
	require.False(t, res.Written)
	require.Equal(t, before, readEntry(t, f, res.URI))
	require.Contains(t, before, "promoted_to: none")
	require.Empty(t, pendingURIs(t, f.svc))
}

// TestWriteJournalEntrySessionSlug pins the session part of the path: only
// path-safe runes survive, capped at eight, with a fallback when none do.
func TestWriteJournalEntrySessionSlug(t *testing.T) {
	f := newFixture(t, true, false)
	for session, want := range map[string]string{
		"../ab/c.d-ef_ghij": "journal/2026-10-10/abcd-ef_-053142.md",
		"//..":              "journal/2026-10-10/session-053142.md",
	} {
		turn := journalTurnAt("x")
		turn.SessionID = session
		res, err := f.svc.WriteJournalEntry(context.Background(), turn)
		require.NoError(t, err)
		require.Equal(t, want, res.URI, session)
	}
}

// TestWriteJournalEntryWriteDisabled keeps the allow_write gate.
func TestWriteJournalEntryWriteDisabled(t *testing.T) {
	f := newFixture(t, false, false)

	_, err := f.svc.WriteJournalEntry(context.Background(), journalTurnAt("x"))

	require.ErrorContains(t, err, "write disabled")
	require.NoDirExists(t, filepath.Join(f.treeRoot, "journal"))
}
