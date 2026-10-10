package memory_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arhuman/mnemos/internal/memory"
	"github.com/arhuman/mnemos/internal/storage"
)

// journalEntry is a captured entry in the shape `hook stop` writes: no
// promoted_to key yet, so the first promotion must add it.
const journalEntry = `---
type: journal
title: Why sqlite
collection: journal
timestamp: "2026-09-24T10:00:00Z"
captured_at: "2026-09-24T10:00:00Z"
session_id: abc12345
---

## Request

Why did we pick sqlite?

## Outcome

Embedded, single file, FTS5.
`

// kbDecision is the kb document a promotion points at.
const kbDecision = "---\ntype: decision\ntitle: Use sqlite\n---\n\n# Use sqlite\n\nEmbedded store.\n"

// seedJournal seeds one journal entry and one kb decision into a fixture.
func seedJournal(t *testing.T, f fixture) (entry, target string) {
	t.Helper()
	entry = f.seed(t, "journal/2026-09-24/abc12345-100000.md", journalEntry, "journal")
	target = f.seed(t, "adr/0001-sqlite.md", kbDecision, "kb")

	return entry, target
}

func pendingURIs(t *testing.T, svc *memory.Service) []string {
	t.Helper()
	got, err := svc.ListPendingJournal(context.Background(), 0)
	require.NoError(t, err)
	uris := make([]string, 0, len(got))
	for _, e := range got {
		uris = append(uris, e.URI)
	}

	return uris
}

// TestJournalPendingListsCapturedEntries covers the read side: a captured
// entry is pending with its capture instant, and a kb document is not.
func TestJournalPendingListsCapturedEntries(t *testing.T) {
	f := newFixture(t, false, false)
	entry, _ := seedJournal(t, f)

	got, err := f.svc.ListPendingJournal(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, entry, got[0].URI)
	require.Equal(t, "journal", got[0].Collection)
	require.Equal(t, "Why sqlite", got[0].Title)
	require.Equal(t, "2026-09-24T10:00:00Z", got[0].CapturedAt)
}

// TestJournalPendingEmptyTier guards the first-run case: no journal is an
// empty list, not an error.
func TestJournalPendingEmptyTier(t *testing.T) {
	f := newFixture(t, false, false)

	got, err := f.svc.ListPendingJournal(context.Background(), 0)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestJournalPendingHidesDeniedCollection applies the visibility boundary to
// the review surface like every other read surface, and applies the limit
// after it so hidden rows never eat into the page.
func TestJournalPendingHidesDeniedCollection(t *testing.T) {
	svc, db, _ := newVisibilityFixture(t, []string{"secret"})
	// newVisibilityFixture chdirs into its tree root.
	root, err := os.Getwd()
	require.NoError(t, err)
	f := fixture{svc: svc, db: db, treeRoot: root}
	// The entry's own collection: frontmatter wins at ingest, so it must say so.
	f.seed(t, "journal/hidden.md", strings.Replace(journalEntry, "collection: journal", "collection: secret", 1), "secret")
	f.seed(t, "journal/visible.md", journalEntry, "journal")

	got, err := svc.ListPendingJournal(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "journal/visible.md", got[0].URI)
}

// TestJournalPromoteStampsAndPreservesBytes is the core contract: the stamp
// adds promoted_to, every other byte of the file survives, and the entry leaves
// the pending list.
func TestJournalPromoteStampsAndPreservesBytes(t *testing.T) {
	f := newFixture(t, true, false)
	entry, target := seedJournal(t, f)
	before := readTree(t, f.treeRoot, entry)

	res, err := f.svc.PromoteJournal(context.Background(), memory.PromoteJournalInput{EntryURI: entry, Target: target})
	require.NoError(t, err)
	require.Equal(t, entry, res.EntryURI)
	require.Equal(t, target, res.PromotedTo)

	after := readTree(t, f.treeRoot, entry)
	want := strings.Replace(before, "session_id: abc12345\n", "session_id: abc12345\npromoted_to: "+target+"\n", 1)
	require.Equal(t, want, after, "only the promoted_to line is added")

	require.Empty(t, pendingURIs(t, f.svc), "a promoted entry leaves the pending list")
	doc, err := storage.GetDocumentByURI(context.Background(), f.db, entry)
	require.NoError(t, err)
	require.True(t, doc.Journal, "the stamp does not change the entry's tier")
	require.Equal(t, target, storage.JournalPromotedTo(*doc))
}

// TestJournalPromoteNoneAndNullKey covers "none" (reviewed, nothing promoted)
// and an entry that already declares promoted_to as null: the key is patched
// in place rather than duplicated.
func TestJournalPromoteNoneAndNullKey(t *testing.T) {
	f := newFixture(t, true, false)
	src := strings.Replace(journalEntry, "session_id: abc12345\n", "session_id: abc12345\npromoted_to:\n", 1)
	entry := f.seed(t, "journal/null.md", src, "journal")
	require.Equal(t, []string{entry}, pendingURIs(t, f.svc), "a null promoted_to is still pending")

	res, err := f.svc.PromoteJournal(context.Background(), memory.PromoteJournalInput{EntryURI: entry, Target: memory.PromoteNone})
	require.NoError(t, err)
	require.Equal(t, "none", res.PromotedTo)

	after := readTree(t, f.treeRoot, entry)
	require.Equal(t, strings.Replace(src, "promoted_to:\n", "promoted_to: none\n", 1), after)
	require.Empty(t, pendingURIs(t, f.svc))
}

// TestJournalPromoteRefusals pins every refusal, and that none of them touch
// the entry on disk.
func TestJournalPromoteRefusals(t *testing.T) {
	f := newFixture(t, true, false)
	entry, target := seedJournal(t, f)
	other := f.seed(t, "journal/other.md", journalEntry, "journal")
	stamped := f.seed(t, "journal/done.md",
		strings.Replace(journalEntry, "session_id: abc12345\n", "session_id: abc12345\npromoted_to: adr/0001-sqlite.md\n", 1), "journal")
	before := readTree(t, f.treeRoot, entry)
	ctx := context.Background()

	cases := []struct {
		name   string
		in     memory.PromoteJournalInput
		target error
		msg    string
	}{
		{"entry is kb", memory.PromoteJournalInput{EntryURI: target, Target: memory.PromoteNone}, memory.ErrNotJournalEntry, "not a journal entry"},
		{"entry unknown", memory.PromoteJournalInput{EntryURI: "journal/nope.md", Target: memory.PromoteNone}, memory.ErrNotJournalEntry, "not a journal entry"},
		{"target missing", memory.PromoteJournalInput{EntryURI: entry, Target: "adr/missing.md"}, memory.ErrInvalidPromoteTarget, "unknown uri"},
		{"target empty", memory.PromoteJournalInput{EntryURI: entry, Target: " "}, memory.ErrInvalidPromoteTarget, "none"},
		{"target is journal", memory.PromoteJournalInput{EntryURI: entry, Target: other}, memory.ErrInvalidPromoteTarget, "itself a journal entry"},
		{"already promoted", memory.PromoteJournalInput{EntryURI: stamped, Target: memory.PromoteNone}, memory.ErrAlreadyPromoted, "adr/0001-sqlite.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.PromoteJournal(ctx, tc.in)
			require.ErrorIs(t, err, tc.target)
			require.Contains(t, err.Error(), tc.msg)
		})
	}
	require.Equal(t, before, readTree(t, f.treeRoot, entry), "a refusal never writes")
}

// TestJournalPromoteRefusesWhenWriteDisabled pins the allow_write gate: the
// refusal fires before any lookup or write.
func TestJournalPromoteRefusesWhenWriteDisabled(t *testing.T) {
	f := newFixture(t, false, false)
	entry, target := seedJournal(t, f)

	_, err := f.svc.PromoteJournal(context.Background(), memory.PromoteJournalInput{EntryURI: entry, Target: target})
	require.Error(t, err)
	require.Contains(t, err.Error(), "allow_write")
	require.Equal(t, []string{entry}, pendingURIs(t, f.svc))
}
