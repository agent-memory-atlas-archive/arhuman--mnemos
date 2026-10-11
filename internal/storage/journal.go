package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/arhuman/mnemos/internal/model"
)

// JournalDocType is the OKF `type` a document declares to be a journal entry.
// Matching is case-insensitive, like every other type comparison in the index,
// so `Journal`, `journal` and `JOURNAL` are one type.
const JournalDocType = "journal"

// IsJournalType reports whether an OKF document type names the journal tier.
// It is the single place the spelling is decided, so ingest, promotion and any
// future surface cannot disagree about what counts as a journal entry.
func IsJournalType(docType string) bool {
	return strings.EqualFold(strings.TrimSpace(docType), JournalDocType)
}

// JournalDoc is a journal row with its review state read by the index:
// PromotedTo is the promoted_to value, "" while the entry is pending.
type JournalDoc struct {
	model.Document

	PromotedTo string
}

// promotedToSQL is the single definition of an entry's review state: the
// trimmed promoted_to value, "" when it is absent, null, blank, or the
// frontmatter is empty or invalid (json_extract raises on invalid JSON, so the
// CASE guards it). A non-string scalar reads back as its text. Pending means
// this expression is "". memory's on-disk check (fieldValue) applies the same
// rule; TestJournalReviewStateAgreesOnDiskAndIndex holds the two together.
const promotedToSQL = `TRIM(CASE WHEN json_valid(frontmatter_json)
	THEN COALESCE(json_extract(frontmatter_json, '$.` + PromotedToKey + `'), '')
	ELSE '' END, char(32, 9, 10, 11, 12, 13))`

// ListJournal returns journal documents ordered newest first, capped at limit
// (a limit <= 0 returns all of them). It reads the stored journal flag rather
// than matching a uri prefix, so an entry stays listed after a move or a rename
// (ADR-0011).
//
// Entries are returned whether or not they have been promoted; PromotedTo on
// each says which. Consolidation filters on it so a second pass does not
// re-promote the same material (ADR-0011).
func ListJournal(ctx context.Context, db *sql.DB, limit int) ([]JournalDoc, error) {
	return queryJournal(ctx, db, "", nil, "DESC", limit)
}

// ListPendingJournal returns the journal entries not yet reviewed (PromotedTo
// is ""), leaving out those whose collection is in hidden. An entry with empty
// or invalid frontmatter has nothing to say it was handled, so it is pending.
// Entries come oldest first, the order consolidation works through them, capped
// at limit (a limit <= 0 returns all of them). The cap applies after the hidden
// filter, so hidden rows never eat into the page. Like ListJournal it keys on
// the stored journal flag, never a uri prefix (ADR-0011).
func ListPendingJournal(ctx context.Context, db *sql.DB, hidden []string, limit int) ([]JournalDoc, error) {
	filter := ` AND ` + promotedToSQL + ` = ''`
	var args []any
	for _, c := range hidden {
		if c == "" {
			continue
		}
		args = append(args, c)
	}
	if len(args) > 0 {
		filter += ` AND COALESCE(collection, '') NOT IN (?` + strings.Repeat(", ?", len(args)-1) + `)`
	}

	return queryJournal(ctx, db, filter, args, "ASC", limit)
}

// queryJournal runs the shared journal listing: journal rows narrowed by the
// extra filter and its args, ordered by COALESCE(modified_at, indexed_at) in
// dir then uri.
func queryJournal(ctx context.Context, db *sql.DB, filter string, args []any, dir string, limit int) ([]JournalDoc, error) {
	q := `SELECT id, uri, collection, content_hash, COALESCE(title, ''),
	             COALESCE(mime_type, ''), size_bytes, COALESCE(modified_at, ''),
	             indexed_at, COALESCE(frontmatter_json, ''), ` + promotedToSQL + `
	      FROM documents WHERE journal = 1` + filter + `
	      ORDER BY COALESCE(modified_at, indexed_at) ` + dir + `, uri`
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list journal: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []JournalDoc
	for rows.Next() {
		var d JournalDoc
		if err := rows.Scan(
			&d.ID, &d.URI, &d.Collection, &d.ContentHash, &d.Title,
			&d.MimeType, &d.SizeBytes, &d.ModifiedAt, &d.IndexedAt, &d.FrontmatterJSON, &d.PromotedTo,
		); err != nil {
			return nil, fmt.Errorf("storage: scan journal row: %w", err)
		}
		d.Journal = true
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate journal rows: %w", err)
	}

	return out, nil
}

// CountJournal returns how many journal documents are indexed. It exists for
// the doctor/stats surfaces, which want the size of the tier without paging
// every row through memory.
func CountJournal(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM documents WHERE journal = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count journal: %w", err)
	}

	return n, nil
}

// PromotedToKey is the frontmatter key a journal entry carries once its content
// has been promoted into the knowledge base, naming the kb document that now
// holds it. Its presence is what stops a second consolidation pass from
// re-promoting the same material (ADR-0011).
const PromotedToKey = "promoted_to"

// CapturedAtKey is the frontmatter key holding when the entry was captured. It
// is distinct from the document's modified_at, which a later edit or a re-ingest
// moves: the capture instant must not drift.
const CapturedAtKey = "captured_at"

// IsJournalURI reports whether the document at uri is a journal entry. A uri
// with no document returns false: absent is not journal.
func IsJournalURI(ctx context.Context, db *sql.DB, uri string) (bool, error) {
	var j bool
	err := db.QueryRowContext(ctx,
		`SELECT journal FROM documents WHERE uri = ?`, uri).Scan(&j)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("storage: journal flag for %q: %w", uri, err)
	default:
		return j, nil
	}
}

// JournalPromotedTo returns the promoted_to value of the indexed document at
// uri, read with the same expression ListPendingJournal filters on: the kb
// document that now holds the entry's content, "none" when nothing was worth
// promoting, or "" when the entry is pending. A uri with no document returns "".
func JournalPromotedTo(ctx context.Context, db *sql.DB, uri string) (string, error) {
	var v string
	err := db.QueryRowContext(ctx,
		`SELECT `+promotedToSQL+` FROM documents WHERE uri = ?`, uri).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("storage: promoted_to for %q: %w", uri, err)
	default:
		return v, nil
	}
}

// JournalCapturedAt returns the entry's CapturedAtKey value, or "" when the
// frontmatter does not carry one.
func JournalCapturedAt(d model.Document) string {
	return frontmatterString(d.FrontmatterJSON, CapturedAtKey)
}

// frontmatterString reads one top-level key from stored frontmatter JSON as
// trimmed text. A non-string scalar is rendered with fmt so a hand-written
// value still reads back non-empty.
func frontmatterString(js, key string) string {
	var m map[string]any
	if js == "" || json.Unmarshal([]byte(js), &m) != nil {
		return ""
	}
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}
