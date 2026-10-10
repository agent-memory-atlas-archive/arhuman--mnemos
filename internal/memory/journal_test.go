package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arhuman/mnemos/internal/config"
	"github.com/arhuman/mnemos/internal/memory"
	"github.com/arhuman/mnemos/internal/storage"
	"github.com/arhuman/mnemos/internal/testutil"
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

// pruneNow is the clock every prune test runs at: thirty days after the
// 2026-09-24 entries were captured.
var pruneNow = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)

// journalAt renders a journal entry captured at captured, stamped with
// promoted_to when promoted is non-empty.
func journalAt(captured, promoted string) string {
	fm := "---\ntype: journal\ntitle: Turn\ncollection: journal\ncaptured_at: \"" + captured + "\"\n"
	if promoted != "" {
		fm += "promoted_to: " + promoted + "\n"
	}

	return fm + "---\n\n## Request\n\nWhy?\n"
}

// pruneTree seeds the canonical prune layout: two old reviewed entries (one per
// stamp kind) alone with a log.md in their dated dir, an old unreviewed entry
// sharing a dir with a log.md, and a young reviewed entry.
type pruneTree struct {
	kbStamped, noneStamped, unreviewed, young, oldLog, keptLog string
}

func seedPruneTree(t *testing.T, f fixture) pruneTree {
	t.Helper()
	target := f.seed(t, "adr/0001-sqlite.md", kbDecision, "kb")

	return pruneTree{
		kbStamped:   f.seed(t, "journal/2026-09-24/aaaa-100000.md", journalAt("2026-09-24T10:00:00Z", target), "journal"),
		noneStamped: f.seed(t, "journal/2026-09-24/bbbb-110000.md", journalAt("2026-09-24T11:00:00Z", memory.PromoteNone), "journal"),
		oldLog:      f.seed(t, "journal/2026-09-24/log.md", "# Log\n\n- created aaaa\n", "kb"),
		unreviewed:  f.seed(t, "journal/2026-09-25/cccc-100000.md", journalAt("2026-09-25T10:00:00Z", ""), "journal"),
		keptLog:     f.seed(t, "journal/2026-09-25/log.md", "# Log\n\n- created cccc\n", "kb"),
		young:       f.seed(t, "journal/2026-10-23/dddd-100000.md", journalAt("2026-10-23T10:00:00Z", memory.PromoteNone), "journal"),
	}
}

func prunedURIs(res memory.PruneJournalResult) []string {
	uris := make([]string, 0, len(res.Pruned))
	for _, e := range res.Pruned {
		uris = append(uris, e.URI)
	}
	slices.Sort(uris)

	return uris
}

func requireIndexed(t *testing.T, f fixture, uri string, want bool) {
	t.Helper()
	doc, err := storage.GetDocumentByURI(context.Background(), f.db, uri)
	require.NoError(t, err)
	require.Equal(t, want, doc != nil, "indexed %s", uri)
}

func requireOnDisk(t *testing.T, f fixture, rel string, want bool) {
	t.Helper()
	_, err := os.Stat(filepath.Join(f.treeRoot, filepath.FromSlash(rel)))
	require.Equal(t, want, err == nil, "on disk %s (stat err %v)", rel, err)
}

// TestJournalPruneDryRunListsOnlyOldReviewed pins the dry run: it lists old
// entries carrying either stamp kind, skips unreviewed and young ones, works
// with allow_delete off, and leaves every file on disk and in the index.
func TestJournalPruneDryRunListsOnlyOldReviewed(t *testing.T) {
	f := newFixture(t, false, false)
	tr := seedPruneTree(t, f)

	res, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: 7 * 24 * time.Hour, Now: pruneNow})
	require.NoError(t, err)
	require.False(t, res.Applied)
	require.Equal(t, []string{tr.kbStamped, tr.noneStamped}, prunedURIs(res))
	require.Equal(t, 1, res.SkippedUnreviewed)
	require.Zero(t, res.SkippedUndated)
	require.Empty(t, res.RemovedDirs)

	for _, uri := range []string{tr.kbStamped, tr.noneStamped, tr.unreviewed, tr.young, tr.oldLog} {
		requireOnDisk(t, f, uri, true)
		requireIndexed(t, f, uri, true)
	}
}

// TestJournalPruneApplyRemovesEligibleAndEmptiedDir covers apply: eligible
// entries leave disk and index, the dated dir left with only log.md goes with
// it (log.md de-indexed too), and the dir still holding an unreviewed entry
// keeps both files, as does the young entry.
func TestJournalPruneApplyRemovesEligibleAndEmptiedDir(t *testing.T) {
	f := newFixture(t, false, true)
	tr := seedPruneTree(t, f)

	res, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: 7 * 24 * time.Hour, Apply: true, Now: pruneNow})
	require.NoError(t, err)
	require.True(t, res.Applied)
	require.Equal(t, []string{tr.kbStamped, tr.noneStamped}, prunedURIs(res))
	require.Equal(t, []string{"journal/2026-09-24"}, res.RemovedDirs)

	for _, uri := range []string{tr.kbStamped, tr.noneStamped, tr.oldLog} {
		requireOnDisk(t, f, uri, false)
		requireIndexed(t, f, uri, false)
	}
	requireOnDisk(t, f, "journal/2026-09-24", false)
	for _, uri := range []string{tr.unreviewed, tr.keptLog, tr.young} {
		requireOnDisk(t, f, uri, true)
		requireIndexed(t, f, uri, true)
	}
	requireOnDisk(t, f, "journal", true)
}

// TestJournalPruneApplyRequiresAllowDelete pins the gate: apply with
// allow_delete off is refused before anything is removed.
func TestJournalPruneApplyRequiresAllowDelete(t *testing.T) {
	f := newFixture(t, true, false)
	tr := seedPruneTree(t, f)

	res, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: time.Hour, Apply: true, Now: pruneNow})
	require.Error(t, err)
	require.Contains(t, err.Error(), "allow_delete")
	require.Empty(t, res.Pruned)
	requireOnDisk(t, f, tr.kbStamped, true)
	requireIndexed(t, f, tr.kbStamped, true)
}

// TestJournalPruneApplyRequiresAllowDeleteWithNothingEligible holds the gate
// when no entry qualifies: an apply must not succeed silently without the
// delete permission just because Forget is never reached.
func TestJournalPruneApplyRequiresAllowDeleteWithNothingEligible(t *testing.T) {
	f := newFixture(t, true, false)
	seedPruneTree(t, f)

	_, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: 100 * 365 * 24 * time.Hour, Apply: true, Now: pruneNow})
	require.Error(t, err)
	require.Contains(t, err.Error(), "allow_delete")
}

// TestJournalPruneKeepsDirWithOtherFiles guards the directory cleanup: a
// non-markdown file left beside log.md keeps the directory and its log.md.
func TestJournalPruneKeepsDirWithOtherFiles(t *testing.T) {
	f := newFixture(t, false, true)
	tr := seedPruneTree(t, f)
	testutil.WriteFile(t, f.treeRoot, "journal/2026-09-24/notes.txt", "keep me\n")

	res, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: 7 * 24 * time.Hour, Apply: true, Now: pruneNow})
	require.NoError(t, err)
	require.Len(t, res.Pruned, 2)
	require.Empty(t, res.RemovedDirs)
	requireOnDisk(t, f, tr.oldLog, true)
	requireIndexed(t, f, tr.oldLog, true)
}

// TestJournalPruneRefusesNonPositiveOlderThan refuses a zero or negative
// threshold, dry run or not, so a typo cannot select the whole tier.
func TestJournalPruneRefusesNonPositiveOlderThan(t *testing.T) {
	f := newFixture(t, false, true)
	tr := seedPruneTree(t, f)

	for _, d := range []time.Duration{0, -time.Hour} {
		for _, apply := range []bool{false, true} {
			_, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: d, Apply: apply, Now: pruneNow})
			require.Error(t, err)
			require.Contains(t, err.Error(), "older-than must be positive")
		}
	}
	requireOnDisk(t, f, tr.kbStamped, true)
}

// TestJournalPruneIgnoresHiddenCollection applies the visibility boundary: an
// old reviewed entry in a denied collection is neither listed nor removed.
func TestJournalPruneIgnoresHiddenCollection(t *testing.T) {
	f := newFixtureWith(t, func(c *config.Config) {
		c.MCP.AllowDelete = true
		c.Security.Visibility.Deny = []string{"secret"}
	})
	hidden := f.seed(t, "journal/2026-09-24/hidden.md",
		strings.Replace(journalAt("2026-09-24T10:00:00Z", memory.PromoteNone), "collection: journal", "collection: secret", 1), "secret")
	visible := f.seed(t, "journal/2026-09-24/visible.md", journalAt("2026-09-24T10:00:00Z", memory.PromoteNone), "journal")

	res, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: time.Hour, Apply: true, Now: pruneNow})
	require.NoError(t, err)
	require.Equal(t, []string{visible}, prunedURIs(res))
	require.Empty(t, res.RemovedDirs, "the hidden entry still occupies the dir")
	requireOnDisk(t, f, hidden, true)
	requireIndexed(t, f, hidden, true)
}

// TestJournalPruneKeepsUndatedEntry counts an entry with no parseable
// captured_at or timestamp as undated and never deletes it, even when reviewed.
func TestJournalPruneKeepsUndatedEntry(t *testing.T) {
	f := newFixture(t, false, true)
	undated := f.seed(t, "journal/undated.md",
		"---\ntype: journal\ntitle: Turn\ncollection: journal\ntimestamp: \"not-a-date\"\ncaptured_at: \"soon\"\npromoted_to: none\n---\n\nBody.\n", "journal")

	res, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: time.Hour, Apply: true, Now: pruneNow})
	require.NoError(t, err)
	require.Empty(t, res.Pruned)
	require.Equal(t, 1, res.SkippedUndated)
	requireOnDisk(t, f, undated, true)
	requireIndexed(t, f, undated, true)
}

// TestJournalPruneFindsMovedEntryByFlag pins ADR-0011's flag-not-path rule: an
// entry living outside journal/ is still pruned, and its directory, which holds
// other content, is kept.
func TestJournalPruneFindsMovedEntryByFlag(t *testing.T) {
	f := newFixture(t, false, true)
	moved := f.seed(t, "archive/old-turn.md", journalAt("2026-09-24T10:00:00Z", memory.PromoteNone), "journal")
	f.seed(t, "archive/readme.md", kbDecision, "kb")

	res, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: time.Hour, Apply: true, Now: pruneNow})
	require.NoError(t, err)
	require.Equal(t, []string{moved}, prunedURIs(res))
	require.Empty(t, res.RemovedDirs)
	requireOnDisk(t, f, moved, false)
	requireIndexed(t, f, moved, false)
	requireOnDisk(t, f, "archive/readme.md", true)
}

// TestJournalPruneKeepsAuthoredIndex guards a moved entry's directory: once the
// entry goes, an index.md left beside log.md is authored structure, so neither
// file nor the directory is removed.
func TestJournalPruneKeepsAuthoredIndex(t *testing.T) {
	f := newFixture(t, false, true)
	moved := f.seed(t, "notes/old-turn.md", journalAt("2026-09-24T10:00:00Z", memory.PromoteNone), "journal")
	testutil.WriteFile(t, f.treeRoot, "notes/index.md", "# Notes\n")
	testutil.WriteFile(t, f.treeRoot, "notes/log.md", "# Log\n")

	res, err := f.svc.PruneJournal(context.Background(), memory.PruneJournalInput{OlderThan: time.Hour, Apply: true, Now: pruneNow})
	require.NoError(t, err)
	require.Equal(t, []string{moved}, prunedURIs(res))
	require.Empty(t, res.RemovedDirs)
	requireOnDisk(t, f, "notes/index.md", true)
	requireOnDisk(t, f, "notes/log.md", true)
}
