package memory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/arhuman/mnemos/internal/okfyaml"
	"github.com/arhuman/mnemos/internal/storage"
)

const (
	// journalTextCap and journalTitleCap bound the request/outcome sections and
	// the title, in runes, so one turn stays a readable entry rather than a dump.
	journalTextCap  = 2000
	journalTitleCap = 80

	// journalFilesCap bounds the "Files touched" list; the rest is counted.
	journalFilesCap = 50

	// journalCollection is the collection every journal entry is indexed under.
	journalCollection = "journal"
)

// JournalTurn is the condensed record of one session turn: the user prompt that
// opened it, when it was sent, the files the assistant edited, and the text of
// the last assistant message. Everything is copied verbatim; nothing is
// summarised.
type JournalTurn struct {
	SessionID string
	Request   string
	At        time.Time
	Files     []string
	Outcome   string
}

// JournalWriteResult reports the entry's uri and whether the file was written.
// Written is false when an already reviewed entry was left untouched.
type JournalWriteResult struct {
	URI     string `json:"uri"`
	Written bool   `json:"written"`
}

// WriteJournalEntry owns the journal entry contract (ADR-0011): it names the
// entry journal/<date>/<session8>-<HHMMSS>.md from the session and the prompt
// instant, renders the frontmatter (type journal, collection journal,
// captured_at = the prompt instant) and body, and writes it through Remember,
// so the write gate, the secret scan and the reindex all apply.
//
// The path is a pure function of the turn, so a second write for the same turn
// updates the entry instead of adding one. An update never drops review state:
// an entry already carrying promoted_to, or one whose frontmatter cannot be
// read back, is left untouched and reported with Written false.
func (s *Service) WriteJournalEntry(ctx context.Context, turn JournalTurn) (JournalWriteResult, error) {
	uri := journalEntryPath(turn.SessionID, turn.At)
	reviewed, err := s.journalEntryReviewed(ctx, uri)
	if err != nil {
		return JournalWriteResult{URI: uri}, err
	}
	if reviewed {
		return JournalWriteResult{URI: uri}, nil
	}
	text, err := renderJournalEntry(turn)
	if err != nil {
		return JournalWriteResult{URI: uri}, err
	}
	if _, err := s.Remember(ctx, RememberInput{
		Type:       storage.JournalDocType,
		Collection: journalCollection,
		Path:       uri,
		Text:       text,
	}); err != nil {
		return JournalWriteResult{URI: uri}, err
	}

	return JournalWriteResult{URI: uri, Written: true}, nil
}

// journalEntryReviewed reads the entry at uri from disk and reports whether it
// must be kept as is: it carries a non-empty promoted_to, or its frontmatter is
// not in a form this code can read. A missing file is not reviewed.
func (s *Service) journalEntryReviewed(ctx context.Context, uri string) (bool, error) {
	abs, _, err := s.resolveWritable(ctx, uri)
	if err != nil {
		return false, fmt.Errorf("journal entry path: %w", err)
	}
	content, err := os.ReadFile(abs) //nolint:gosec // abs is confined to the tree by resolveWritable
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("journal entry read: %w", err)
	}
	doc, ok, err := okfyaml.Parse(content)
	if err != nil || !ok {
		return true, nil //nolint:nilerr // an unreadable entry is kept, never overwritten
	}
	for _, f := range doc.Fields() {
		if f.Key == storage.PromotedToKey && strings.TrimSpace(f.Value) != "" {
			return true, nil
		}
	}

	return false, nil
}

// journalEntryPath names the entry for a turn: journal/<date>/<session8>-<HHMMSS>.md,
// where session8 is the first eight path-safe runes of the session id.
func journalEntryPath(sessionID string, at time.Time) string {
	return "journal/" + at.Format("2006-01-02") + "/" + sessionSlug(sessionID) + "-" + at.Format("150405") + ".md"
}

// sessionSlug keeps the first eight ASCII letters, digits, '-' or '_' of id, or
// "session" when none survive.
func sessionSlug(id string) string {
	var b strings.Builder
	for _, r := range id {
		if b.Len() == 8 {
			break
		}
		if r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			_, _ = b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "session"
	}

	return b.String()
}

// journalFrontmatter is the authored frontmatter of a journal entry. Field order
// is the rendered key order; yaml.Marshal does the quoting.
type journalFrontmatter struct {
	Type       string `yaml:"type"`
	Title      string `yaml:"title"`
	Collection string `yaml:"collection"`
	Timestamp  string `yaml:"timestamp"`
	CapturedAt string `yaml:"captured_at"`
	SessionID  string `yaml:"session_id"`
}

// renderJournalEntry renders the turn as an OKF document with its own
// frontmatter block, so Remember's PrepareOKF keeps it verbatim.
func renderJournalEntry(turn JournalTurn) (string, error) {
	at := turn.At.Format(time.RFC3339)
	fm, err := yaml.Marshal(journalFrontmatter{
		Type:       storage.JournalDocType,
		Title:      journalTitle(turn.Request),
		Collection: journalCollection,
		Timestamp:  at,
		CapturedAt: at,
		SessionID:  turn.SessionID,
	})
	if err != nil {
		return "", fmt.Errorf("journal entry frontmatter: %w", err)
	}

	var b strings.Builder
	_, _ = b.WriteString("---\n")
	_, _ = b.Write(fm)
	_, _ = b.WriteString("---\n\n## Request\n\n")
	_, _ = b.WriteString(capRunes(turn.Request, journalTextCap))
	_, _ = b.WriteString("\n\n## Files touched\n\n")
	if len(turn.Files) == 0 {
		_, _ = b.WriteString("none\n")
	}
	for i, f := range turn.Files {
		if i == journalFilesCap {
			_, _ = fmt.Fprintf(&b, "- +%d more\n", len(turn.Files)-journalFilesCap)

			break
		}
		_, _ = fmt.Fprintf(&b, "- %s\n", f)
	}
	_, _ = b.WriteString("\n## Outcome\n\n")
	if turn.Outcome == "" {
		_, _ = b.WriteString("none\n")
	} else {
		_, _ = b.WriteString(capRunes(turn.Outcome, journalTextCap) + "\n")
	}

	return b.String(), nil
}

// journalTitle is the request's first non-empty line, capped for a title.
func journalTitle(request string) string {
	for line := range strings.SplitSeq(request, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return capRunes(line, journalTitleCap)
		}
	}

	return "journal entry"
}

// capRunes truncates s to at most n runes, appending a marker when it clips.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}

	return string([]rune(s)[:n]) + " [truncated]"
}
