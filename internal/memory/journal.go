package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/arhuman/mnemos/internal/chunk"
	"github.com/arhuman/mnemos/internal/ingest"
	"github.com/arhuman/mnemos/internal/model"
	"github.com/arhuman/mnemos/internal/okfyaml"
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
// entry that already carries promoted_to is refused (ErrAlreadyPromoted).
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
	if err := s.checkPromoteTarget(ctx, target); err != nil {
		return PromoteJournalResult{}, err
	}

	if err := s.stampPromotedTo(ctx, entry.URI, target); err != nil {
		return PromoteJournalResult{EntryURI: entry.URI}, err
	}

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

// stampPromotedTo writes promoted_to into the entry's frontmatter and reindexes
// it. It owns its write path rather than calling EditFrontmatter because
// okfyaml.PatchScalar only rewrites an existing key, and a captured entry has
// no promoted_to until its first promotion.
func (s *Service) stampPromotedTo(ctx context.Context, uri, target string) error {
	abs, resolved, err := s.resolveWritable(ctx, uri)
	if err != nil {
		return fmt.Errorf("edit path: %w", err)
	}
	current, err := os.ReadFile(abs) //nolint:gosec // abs is confined to the tree by ResolveWithin
	if err != nil {
		return fmt.Errorf("edit read %q: %w", resolved, err)
	}

	content, err := setScalar(current, storage.PromotedToKey, target)
	if err != nil {
		return err
	}

	if _, err = ingest.WriteFileAtomic(abs, content); err != nil {
		return fmt.Errorf("edit write %q: %w", resolved, err)
	}
	if _, _, err = ingest.ReindexOne(ctx, s.db, s.logger, abs, resolved, s.collectionFor(ctx, resolved),
		chunk.ConfigFrom(s.cfg.Chunking.TargetTokens, s.cfg.Chunking.OverlapTokens)); err != nil {
		return fmt.Errorf("%w: %w", ErrReindexAfterWrite, err)
	}

	s.logger.Info("journal entry promoted", "uri", resolved, "promoted_to", target)

	return nil
}

// setScalar sets key to value in content's frontmatter: patched in place when
// the key exists, otherwise appended as the block's last line. The result is
// re-parsed and must read the value back, so a misplaced insert is refused
// rather than written.
func setScalar(content []byte, key, value string) ([]byte, error) {
	doc, ok, err := okfyaml.Parse(content)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotEditable, err)
	}
	if !ok {
		return nil, ErrNotEditable
	}

	var out []byte
	if _, present := field(doc, key); present {
		if out, err = doc.PatchScalar(key, value); err != nil {
			return nil, fmt.Errorf("edit field %q: %w", key, err)
		}
	} else {
		line, merr := yaml.Marshal(map[string]string{key: value})
		if merr != nil {
			return nil, fmt.Errorf("edit field %q: %w", key, merr)
		}
		// Parse accepted the canonical form, so the block ends in its closing
		// "---" line and everything before it ends in a newline.
		block := content[:len(content)-len(doc.Body())]
		at := bytes.LastIndex(block, []byte("---"))
		out = append(append(append([]byte{}, block[:at]...), line...), content[at:]...)
	}

	check, ok, err := okfyaml.Parse(out)
	if got, _ := field(check, key); err != nil || !ok || got != value {
		return nil, fmt.Errorf("%w: could not set %q", ErrNotEditable, key)
	}

	return out, nil
}

// field returns key's displayed value and whether the frontmatter defines it.
func field(doc okfyaml.Doc, key string) (string, bool) {
	for _, f := range doc.Fields() {
		if f.Key == key {
			return f.Value, true
		}
	}

	return "", false
}
