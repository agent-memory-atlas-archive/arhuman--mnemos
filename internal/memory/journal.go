package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/arhuman/mnemos/internal/model"
	"github.com/arhuman/mnemos/internal/storage"
)

// PromoteNone is the promotion target recording that a journal entry was
// reviewed and held nothing worth promoting into the knowledge base.
const PromoteNone = "none"

// ErrNotJournalEntry reports a promotion whose entry uri is not an indexed
// journal entry (ADR-0011: identified by the stored flag, never a path).
var ErrNotJournalEntry = errors.New("journal: not a journal entry")

// ErrInvalidPromoteTarget reports a promotion target that is neither "none" nor
// an indexed, visible kb document. A journal entry is never a valid target:
// promotion moves content out of the journal, not sideways within it.
var ErrInvalidPromoteTarget = errors.New("journal: invalid promotion target")

// ErrAlreadyPromoted reports an entry that already carries promoted_to. It is
// refused rather than re-stamped, so a second pass cannot rewrite the record of
// where the content went.
var ErrAlreadyPromoted = errors.New("journal: entry already promoted")

// JournalEntry is one journal entry as the review surfaces show it. CapturedAt
// is the frontmatter capture instant (empty when the entry declares none);
// ModifiedAt is the file's, which a later edit moves.
type JournalEntry struct {
	URI        string `json:"uri"`
	Collection string `json:"collection"`
	Title      string `json:"title"`
	CapturedAt string `json:"captured_at"`
	ModifiedAt string `json:"modified_at"`
}

// ListPendingJournal returns the journal entries not yet reviewed, oldest
// first, capped at limit (a limit <= 0 returns all of them). It is read-only and
// ungated; entries in a hidden collection ([security].visibility.deny) are
// dropped like on every other read surface. An empty tier is an empty slice,
// never an error.
func (s *Service) ListPendingJournal(ctx context.Context, limit int) ([]JournalEntry, error) {
	// The cap is applied after the visibility filter so hidden rows cannot eat
	// into the page a caller asked for.
	docs, err := storage.ListPendingJournal(ctx, s.db, 0)
	if err != nil {
		return nil, fmt.Errorf("memory: pending journal: %w", err)
	}

	out := make([]JournalEntry, 0, len(docs))
	for _, d := range docs {
		if s.hiddenCollection(d.Collection) {
			continue
		}
		out = append(out, JournalEntry{
			URI:        d.URI,
			Collection: d.Collection,
			Title:      d.Title,
			CapturedAt: storage.JournalCapturedAt(d),
			ModifiedAt: d.ModifiedAt,
		})
		if limit > 0 && len(out) == limit {
			break
		}
	}

	return out, nil
}

// PromoteJournalInput names the entry to stamp and where its content went:
// Target is the uri of the kb document that now holds it, or PromoteNone.
type PromoteJournalInput struct {
	EntryURI string
	Target   string
}

// PromoteJournalResult reports the stamped entry and the recorded target.
type PromoteJournalResult struct {
	EntryURI   string `json:"entry_uri"`
	PromotedTo string `json:"promoted_to"`
}

// PromoteJournal records that a journal entry was handled by stamping its
// promoted_to frontmatter key with Target, then reindexes it so it leaves the
// pending list. It is gated by [mcp].allow_write. It never writes the kb
// document itself: the caller writes that first (mnemos.remember) and then
// records it here (ADR-0011: promotion is a deliberate act, never automatic).
//
// Refusals: EntryURI must be an indexed, visible journal entry
// (ErrNotJournalEntry); Target must be PromoteNone or an indexed, visible
// document that is not itself a journal entry (ErrInvalidPromoteTarget); an
// entry that already carries promoted_to, in its index row or in the file on
// disk (checked inside the edit, so a stale index cannot let it through), is
// refused (ErrAlreadyPromoted).
//
// Every other frontmatter byte and the body are kept verbatim. The file is
// written before the reindex; a reindex failure wraps ErrReindexAfterWrite, as
// EditFrontmatter does.
func (s *Service) PromoteJournal(ctx context.Context, in PromoteJournalInput) (PromoteJournalResult, error) {
	if err := ctx.Err(); err != nil {
		return PromoteJournalResult{}, err
	}
	if !s.cfg.MCP.AllowWrite {
		return PromoteJournalResult{}, errWriteDisabled
	}

	entry, err := s.visibleDoc(ctx, strings.TrimSpace(in.EntryURI))
	if err != nil {
		return PromoteJournalResult{}, err
	}
	if entry == nil || !entry.Journal {
		return PromoteJournalResult{}, fmt.Errorf("%w: %q", ErrNotJournalEntry, in.EntryURI)
	}
	if prev := storage.JournalPromotedTo(*entry); prev != "" {
		return PromoteJournalResult{}, fmt.Errorf("%w: %q has promoted_to %q", ErrAlreadyPromoted, entry.URI, prev)
	}

	target := strings.TrimSpace(in.Target)
	if err = s.checkPromoteTarget(ctx, target); err != nil {
		return PromoteJournalResult{}, err
	}

	_, err = s.EditFrontmatter(ctx, EditFrontmatterInput{
		URI:    entry.URI,
		Fields: []FieldEdit{{Key: storage.PromotedToKey, Value: target, InsertOnly: true}},
	})
	if errors.Is(err, ErrFieldSet) {
		return PromoteJournalResult{EntryURI: entry.URI}, fmt.Errorf("%w: %q on disk: %w", ErrAlreadyPromoted, entry.URI, err)
	}
	if err != nil {
		return PromoteJournalResult{EntryURI: entry.URI}, err
	}
	s.logger.Info("journal entry promoted", "uri", entry.URI, "promoted_to", target)

	return PromoteJournalResult{EntryURI: entry.URI, PromotedTo: target}, nil
}

// visibleDoc returns the indexed document at uri, or nil when there is none or
// it sits in a hidden collection (a hidden document is reported as missing).
func (s *Service) visibleDoc(ctx context.Context, uri string) (*model.Document, error) {
	doc, err := storage.GetDocumentByURI(ctx, s.db, uri)
	if err != nil {
		return nil, fmt.Errorf("memory: journal lookup: %w", err)
	}
	if doc == nil || s.hiddenCollection(doc.Collection) {
		return nil, nil //nolint:nilnil // nil document is the documented "not found" result
	}

	return doc, nil
}

// checkPromoteTarget accepts PromoteNone or the uri of an indexed, visible,
// non-journal document.
func (s *Service) checkPromoteTarget(ctx context.Context, target string) error {
	if target == PromoteNone {
		return nil
	}
	if target == "" {
		return fmt.Errorf("%w: provide a kb document uri or %q", ErrInvalidPromoteTarget, PromoteNone)
	}
	doc, err := s.visibleDoc(ctx, target)
	if err != nil {
		return err
	}
	if doc == nil {
		return fmt.Errorf("%w: unknown uri %q (write the kb document first)", ErrInvalidPromoteTarget, target)
	}
	if doc.Journal {
		return fmt.Errorf("%w: %q is itself a journal entry", ErrInvalidPromoteTarget, target)
	}

	return nil
}

// PruneJournalInput selects which reviewed journal entries to delete. OlderThan
// must be positive; Apply false is a dry run; a zero Now means time.Now().
type PruneJournalInput struct {
	OlderThan time.Duration
	Apply     bool
	Now       time.Time
}

// PruneJournalResult reports a prune. Pruned lists the entries removed (or, in
// a dry run, the ones that would be); RemovedDirs lists the tree-relative
// directories deleted because the prune left only their log.md in them.
type PruneJournalResult struct {
	Pruned            []JournalEntry `json:"pruned"`
	SkippedUnreviewed int            `json:"skipped_unreviewed"`
	SkippedUndated    int            `json:"skipped_undated"`
	RemovedDirs       []string       `json:"removed_dirs"`
	Applied           bool           `json:"applied"`
}

// PruneJournal deletes reviewed journal entries older than in.OlderThan. It is
// the explicit, human-initiated cleanup ADR-0011 allows: nothing calls it on a
// timer.
//
// Candidates are found by the stored journal flag, never a path, so a moved
// entry is still pruned; entries in a hidden collection are ignored. An entry's
// age is now minus its captured_at, falling back to its modified_at; one with
// neither parseable (RFC 3339) is kept and counted in SkippedUndated. Only an
// entry stamped with promoted_to (a kb uri or PromoteNone) whose age is strictly
// greater than OlderThan is eligible. An unreviewed entry is never pruned; one old
// enough to qualify is counted in SkippedUnreviewed. Candidates are listed in uri
// order.
//
// A dry run (Apply false) only reads and is ungated. Apply requires
// [mcp].allow_delete and refuses before any read when it is off. Each entry is
// removed through Forget (index first, then disk); the first failure stops the
// removals. Then every directory that held a removed entry and now holds nothing
// but its log.md loses it and is removed; a directory holding anything else, an
// index.md included, is left untouched. That cleanup also runs after a failed
// removal, and the error is returned with what was removed so far. An
// OlderThan <= 0 is refused so a typo cannot empty the tier. Every error carries
// the "journal prune:" prefix.
func (s *Service) PruneJournal(ctx context.Context, in PruneJournalInput) (PruneJournalResult, error) {
	res := PruneJournalResult{Pruned: []JournalEntry{}, RemovedDirs: []string{}, Applied: in.Apply}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if in.Apply && !s.cfg.MCP.AllowDelete {
		return res, fmt.Errorf("journal prune: %w", errDeleteDisabled)
	}
	if in.OlderThan <= 0 {
		return res, fmt.Errorf("journal prune: older-than must be positive, got %s", in.OlderThan)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	docs, err := storage.ListJournal(ctx, s.db, 0)
	if err != nil {
		return res, fmt.Errorf("journal prune: %w", err)
	}
	s.planPrune(&res, docs, in.OlderThan, now)
	if !in.Apply {
		return res, nil
	}

	return s.applyPrune(ctx, res)
}

// planPrune sorts the visible journal rows of docs into res: eligible entries
// in Pruned (uri order), and the unreviewed and undated skip counts. It reads
// nothing but the rows.
func (s *Service) planPrune(res *PruneJournalResult, docs []model.Document, olderThan time.Duration, now time.Time) {
	for _, d := range docs {
		if s.hiddenCollection(d.Collection) {
			continue
		}
		capturedAt, promotedTo := journalMeta(d)
		age, dated := journalAge(capturedAt, d.ModifiedAt, now)
		switch {
		case !dated:
			res.SkippedUndated++
		case age <= olderThan:
		case promotedTo == "":
			res.SkippedUnreviewed++
		default:
			res.Pruned = append(res.Pruned, JournalEntry{
				URI:        d.URI,
				Collection: d.Collection,
				Title:      d.Title,
				CapturedAt: capturedAt,
				ModifiedAt: d.ModifiedAt,
			})
		}
	}
	slices.SortFunc(res.Pruned, func(a, b JournalEntry) int { return strings.Compare(a.URI, b.URI) })
}

// applyPrune forgets the planned entries in order, stopping at the first
// failure, then removes the directories the removed entries left holding only a
// log.md. res.Pruned is rewritten to the entries actually removed.
func (s *Service) applyPrune(ctx context.Context, res PruneJournalResult) (PruneJournalResult, error) {
	candidates := res.Pruned
	res.Pruned = []JournalEntry{}
	dirs := make(map[string]bool)
	var forgetErr error
	for _, e := range candidates {
		if _, forgetErr = s.Forget(ctx, e.URI); forgetErr != nil {
			break
		}
		res.Pruned = append(res.Pruned, e)
		if dir := path.Dir(e.URI); dir != "." {
			dirs[dir] = true
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		removed, err := s.removeIfOnlyLog(ctx, dir)
		if err != nil {
			return res, fmt.Errorf("journal prune: %w", errors.Join(forgetErr, err))
		}
		if removed {
			res.RemovedDirs = append(res.RemovedDirs, dir)
		}
	}
	if forgetErr != nil {
		return res, fmt.Errorf("journal prune: %w", forgetErr)
	}

	return res, nil
}

// journalMeta decodes a journal row's frontmatter once and returns its
// captured_at and promoted_to values, each "" when absent. A non-string scalar
// is rendered with fmt, as storage's pending test does.
func journalMeta(d model.Document) (capturedAt, promotedTo string) {
	var m map[string]any
	if d.FrontmatterJSON == "" || json.Unmarshal([]byte(d.FrontmatterJSON), &m) != nil {
		return "", ""
	}
	str := func(key string) string {
		if v, ok := m[key]; ok && v != nil {
			return strings.TrimSpace(fmt.Sprint(v))
		}

		return ""
	}

	return str(storage.CapturedAtKey), str(storage.PromotedToKey)
}

// journalAge returns now minus capturedAt, else modifiedAt, and false when
// neither parses as RFC 3339.
func journalAge(capturedAt, modifiedAt string, now time.Time) (time.Duration, bool) {
	for _, ts := range []string{capturedAt, modifiedAt} {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return now.Sub(t), true
		}
	}

	return 0, false
}

// removeIfOnlyLog deletes the tree directory dir when its only entry is the
// log.md that Remember keeps for the entries written there, removing it through
// Forget so an indexed log.md leaves the index too. Any other entry keeps the
// directory, index.md included: it is authored structure, never prune output.
func (s *Service) removeIfOnlyLog(ctx context.Context, dir string) (bool, error) {
	abs, _, err := s.resolveWritable(ctx, dir)
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return false, fmt.Errorf("read dir %q: %w", dir, err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.EqualFold(e.Name(), "log.md") {
			return false, nil
		}
	}
	for _, e := range entries {
		if _, err := s.Forget(ctx, path.Join(dir, e.Name())); err != nil {
			return false, err
		}
	}
	if err := os.Remove(abs); err != nil {
		return false, fmt.Errorf("remove dir %q: %w", dir, err)
	}
	s.logger.Info("journal prune removed directory", "dir", dir)

	return true, nil
}
