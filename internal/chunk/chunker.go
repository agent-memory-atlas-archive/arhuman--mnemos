// Package chunk turns a ParsedDoc into deterministic, line-anchored chunks.
// Section boundaries (heading, declaration, paragraph) are decided upstream by
// the parser; this package only windows each section to a token budget
// (target/overlap) so chunk sizing stays config-driven.
package chunk

import (
	"encoding/json"
	"strings"

	"github.com/arhuman/mnemos/internal/model"
)

// Config carries the token budget that bounds chunk sizing. Sections estimated
// above TargetTokens are split into windows of roughly TargetTokens with
// OverlapTokens of carry-over; smaller sections stay whole.
type Config struct {
	TargetTokens  int
	OverlapTokens int
}

// ConfigFrom builds a Config from target and overlap token counts. It is the
// single place configuration values become a chunk.Config, so callers (ingest,
// capture, move, eval) stop re-assembling the struct field by field and the
// chunk package owns the constructor for its own type.
func ConfigFrom(targetTokens, overlapTokens int) Config {
	return Config{TargetTokens: targetTokens, OverlapTokens: overlapTokens}
}

// Dispatch turns doc into ordered chunks, windowing each section to cfg's
// token budget. StructureOnly documents (e.g. OKF index.md) yield no chunks.
// Deterministic: the same ParsedDoc always yields identical chunks, which
// keeps re-ingest idempotent and golden tests stable.
func Dispatch(doc model.ParsedDoc, cfg Config) []model.Chunk {
	if doc.StructureOnly {
		return nil
	}

	var tc wordEstimator
	meta := chunkMetadata(doc)
	tags := joinTags(doc)

	var chunks []model.Chunk
	ordinal := 0
	for _, sec := range doc.Sections {
		for _, w := range wholeOrWindows(sec, tc, cfg) {
			chunks = append(chunks, model.Chunk{
				Ordinal:      ordinal,
				HeadingPath:  sec.HeadingPath,
				Content:      w.content,
				Tags:         tags,
				DocType:      doc.DocType,
				TokenCount:   tc.Count(w.content),
				StartLine:    w.startLine,
				EndLine:      w.endLine,
				MetadataJSON: meta,
			})
			ordinal++
		}
	}

	return chunks
}

// window is a contiguous slice of a section with its exact 1-based line span.
type window struct {
	content   string
	startLine int
	endLine   int
}

// wholeOrWindows returns the section as a single window when it fits the token
// budget (or budgeting is disabled), otherwise line windows of ~target tokens
// with overlap carry-over. It is the shared split body for every chunker family:
// they differ only in how upstream parsing carved the sections, not in how an
// oversized section is windowed.
func wholeOrWindows(sec model.Section, tc wordEstimator, cfg Config) []window {
	if cfg.TargetTokens <= 0 || tc.Count(sec.Content) <= cfg.TargetTokens {
		return []window{{content: sec.Content, startLine: sec.StartLine, endLine: sec.EndLine}}
	}

	return windowByTokens(splitLines(sec.Content), sec.StartLine, tc, cfg.TargetTokens, cfg.OverlapTokens)
}

// windowByTokens packs lines into windows of ~target tokens with overlap lines
// of carry-over, anchoring each window to its absolute 1-based line range
// (firstLine corresponds to lines[0]). The overlap is expressed in tokens but
// applied by rewinding whole lines so ranges stay exact.
func windowByTokens(lines []string, firstLine int, tc wordEstimator, target, overlap int) []window {
	var out []window
	n := len(lines)
	i := 0
	for i < n {
		start := i
		tokens := 0
		j := i
		for j < n {
			lineTokens := tc.Count(lines[j])
			if j > start && tokens+lineTokens > target {
				break
			}
			tokens += lineTokens
			j++
		}
		// [start, j) is the current window of lines.
		content := strings.Join(lines[start:j], "\n")
		out = append(out, window{
			content:   content,
			startLine: firstLine + start,
			endLine:   firstLine + j - 1,
		})
		if j >= n {
			break
		}
		// Rewind whole lines to honor the token overlap for the next window.
		next := j
		back := 0
		for k := j - 1; k > start && back < overlap; k-- {
			back += tc.Count(lines[k])
			next = k
		}
		// Always make forward progress.
		if next <= start {
			next = start + 1
		}
		i = next
	}

	return out
}

// splitLines splits content into lines without dropping a trailing empty line
// distinction relevant to range math.
func splitLines(content string) []string {
	return strings.Split(content, "\n")
}

// joinTags renders doc.Tags as the space-joined FTS tags column.
func joinTags(doc model.ParsedDoc) string {
	return strings.Join(doc.Tags, " ")
}

// chunkMetadata builds the per-chunk metadata_json from the document-level stash
// (e.g. the frontmatter `resource` key). Returns "" when empty so the DB column
// stays NULL-ish.
func chunkMetadata(doc model.ParsedDoc) string {
	if len(doc.Metadata) == 0 {
		return ""
	}
	b, err := json.Marshal(doc.Metadata)
	if err != nil {
		return ""
	}

	return string(b)
}
