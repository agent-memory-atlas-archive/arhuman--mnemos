package okfyaml

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// span locates the source bytes of a single-line node within the frontmatter.
func (d Doc) span(node *yaml.Node) (start, end int, err error) {
	lineStart, lineEnd, err := d.lineBounds(node.Line)
	if err != nil {
		return 0, 0, err
	}
	start = lineStart + node.Column - 1
	if start < lineStart || start > lineEnd {
		return 0, 0, fmt.Errorf("okfyaml: node at line %d column %d is outside its line", node.Line, node.Column)
	}
	end, err = valueEnd(d.inner[start:lineEnd], node)
	if err != nil {
		return 0, 0, err
	}

	return start, start + end, nil
}

// blockSeqSpan returns the whole-line span covering a block sequence's items.
func (d Doc) blockSeqSpan(node *yaml.Node) (start, end int, err error) {
	if len(node.Content) == 0 {
		return 0, 0, errors.New("okfyaml: cannot locate an empty block list")
	}
	first, last := node.Content[0], node.Content[len(node.Content)-1]
	for _, item := range node.Content {
		if item.Kind != yaml.ScalarNode {
			return 0, 0, fmt.Errorf("okfyaml: list at line %d has a non-scalar item", node.Line)
		}
	}
	start, _, err = d.lineBounds(first.Line)
	if err != nil {
		return 0, 0, err
	}
	_, lastEnd, err := d.lineBounds(last.Line)
	if err != nil {
		return 0, 0, err
	}
	if lastEnd < len(d.inner) {
		lastEnd++ // include the item's newline; the replacement supplies its own
	}

	return start, lastEnd, nil
}

// lineBounds returns the byte offsets of a 1-based line's first byte and of its
// newline (or of end-of-input for an unterminated last line).
func (d Doc) lineBounds(line int) (start, end int, err error) {
	if line < 1 || line > len(d.lines) {
		return 0, 0, fmt.Errorf("okfyaml: line %d is outside the frontmatter", line)
	}
	start = d.lines[line-1]
	end = len(d.inner)
	if idx := bytes.IndexByte(d.inner[start:], '\n'); idx >= 0 {
		end = start + idx
	}

	return start, end, nil
}

// valueEnd returns the offset within line (which starts at the node's first
// byte and ends at the newline) just past the node's own text, leaving any
// padding and trailing comment to the caller.
func valueEnd(line []byte, node *yaml.Node) (int, error) {
	switch {
	case node.Style&yaml.DoubleQuotedStyle != 0:
		return quotedEnd(line, '"', true)
	case node.Style&yaml.SingleQuotedStyle != 0:
		return quotedEnd(line, '\'', false)
	case node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
		return 0, fmt.Errorf("okfyaml: multi-line block scalar at line %d is not editable", node.Line)
	case node.Style&yaml.FlowStyle != 0:
		return flowEnd(line)
	}

	return len(bytes.TrimRight(line[:commentStart(line)], " \t")), nil
}

// commentStart returns the offset of a trailing "#" comment within line, or
// len(line) when there is none. A "#" only opens a comment when it starts the
// text or follows whitespace, which is exactly where a plain scalar ends.
func commentStart(line []byte) int {
	for i, c := range line {
		if c == '#' && (i == 0 || isSpace(line[i-1])) {
			return i
		}
	}

	return len(line)
}

// quotedEnd returns the offset just past a quoted scalar opening at line[0].
// Double quotes escape with a backslash; single quotes escape by doubling.
func quotedEnd(line []byte, quote byte, backslash bool) (int, error) {
	if len(line) == 0 || line[0] != quote {
		return 0, errors.New("okfyaml: quoted value does not start with its quote")
	}
	for i := 1; i < len(line); i++ {
		switch {
		case backslash && line[i] == '\\':
			i++
		case line[i] != quote:
		case !backslash && i+1 < len(line) && line[i+1] == quote:
			i++
		default:
			return i + 1, nil
		}
	}

	return 0, errors.New("okfyaml: quoted value is not closed on its line")
}

// flowEnd returns the offset just past a flow collection opening at line[0],
// tracking nesting and quoted items.
func flowEnd(line []byte) (int, error) {
	depth := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		case '"', '\'':
			n, err := quotedEnd(line[i:], line[i], line[i] == '"')
			if err != nil {
				return 0, err
			}
			i += n - 1
		default:
		}
	}

	return 0, errors.New("okfyaml: flow collection is not closed on its line")
}

// spanMatches reports whether src re-parses to the same value as node. It is
// the guard that makes a positional patch safe: a span this package located
// wrongly will not round-trip, and the patch is refused instead of applied.
func spanMatches(src []byte, node *yaml.Node) bool {
	if len(src) == 0 {
		return node.Kind == yaml.ScalarNode && node.Tag == "!!null"
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return false
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return false
	}
	got := doc.Content[0]
	if got.Kind != node.Kind {
		return false
	}
	if got.Kind == yaml.ScalarNode {
		return got.Value == node.Value
	}

	return slicesEqual(seqValues(got), seqValues(node))
}

// lineOffsets returns the byte offset at which each line of b starts.
func lineOffsets(b []byte) []int {
	offsets := []int{0}
	for i, c := range b {
		if c == '\n' && i+1 < len(b) {
			offsets = append(offsets, i+1)
		}
	}

	return offsets
}

// replaceSpan returns src with [start,end) replaced by text.
func replaceSpan(src []byte, start, end int, text string) []byte {
	out := make([]byte, 0, len(src)-(end-start)+len(text))
	out = append(out, src[:start]...)
	out = append(out, text...)

	return append(out, src[end:]...)
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' }
