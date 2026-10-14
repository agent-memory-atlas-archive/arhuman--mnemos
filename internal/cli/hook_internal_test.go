package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSalientTermsStripsStopwordsAndShortTokens(t *testing.T) {
	got := salientTerms("Why did we choose staging on the VPS?")
	// why/did/the are stopwords; "we"/"on" are shorter than 3 runes; the rest are
	// lowercased content words in first-seen order.
	require.Equal(t, []string{"choose", "staging", "vps"}, got)
}

func TestSalientTermsDeduplicatesAndCaps(t *testing.T) {
	require.Equal(t, []string{"alpha", "beta"}, salientTerms("alpha beta alpha BETA alpha"))

	long := strings.Repeat("aaa bbb ccc ddd eee fff ggg hhh iii jjj ", 1)
	require.Len(t, salientTerms(long), 8, "capped at maxTerms")
}

func TestShouldRecall(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   bool
	}{
		{"empty", "   ", false},
		{"plain imperative", "add a csv export button", false},
		{"question", "which port does the gateway bind?", true},
		{"cue phrase", "the usual staging setup please", true},
		{"french cue", "c'était quoi le port déjà", true},
		{"oversized paste", strings.Repeat("x", hookRecallMaxPromptChars+1) + "?", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, shouldRecall(tc.prompt))
		})
	}
}

func TestParsePrompt(t *testing.T) {
	require.Equal(t, "hello there", parsePrompt([]byte(`{"prompt":"  hello there  ","hook_event_name":"UserPromptSubmit"}`)))
	require.Equal(t, "", parsePrompt([]byte(`not json at all`)))
	require.Equal(t, "", parsePrompt([]byte(`{"cwd":"/tmp"}`)))
}

func TestCapTokens(t *testing.T) {
	small := "line one\nline two\n"
	require.Equal(t, small, capTokens(small, 100), "under budget is unchanged")
	require.Equal(t, small, capTokens(small, 0), "zero budget is uncapped")

	big := strings.Repeat("abcd\n", 200) // 1000 bytes
	out := capTokens(big, 10)            // ~40 char budget
	require.Less(t, len(out), len(big))
	require.Contains(t, out, "truncated to ~10 tokens")
	require.True(t, strings.HasSuffix(strings.TrimRight(out, "\n"), "tokens]_"))
}

func TestWorkingSetRef(t *testing.T) {
	require.Equal(t, "(tasks/a.md)", workingSetRef(false, "mnemos", "tasks/a.md"))
	require.Equal(t, "(mnemos: tasks/a.md)", workingSetRef(true, "mnemos", "tasks/a.md"))
	require.Equal(t, "(tasks/a.md)", workingSetRef(true, "", "tasks/a.md"), "no collection, no label")
}

// BenchmarkReadLastTurn reads the last turn of a 32 MiB transcript (2000
// assistant lines of about 16 KiB) in three shapes: many short turns, where the
// backward scan stops early; one long turn, and no real prompt at all, where it
// reads the whole file.
func BenchmarkReadLastTurn(b *testing.B) {
	shapes := []struct {
		name    string
		prompts func(i int) bool
		want    bool
	}{
		{"many-turns", func(int) bool { return true }, true},
		{"one-long-turn", func(i int) bool { return i == 0 }, true},
		{"no-prompt", func(int) bool { return false }, false},
	}
	for _, sh := range shapes {
		b.Run(sh.name, func(b *testing.B) {
			path := writeBenchTranscript(b, sh.prompts)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, ok := readLastTurn(path, ""); ok != sh.want {
					b.Fatalf("readLastTurn ok = %v, want %v", ok, sh.want)
				}
			}
		})
	}
}

// writeBenchTranscript writes 2000 assistant lines of about 16 KiB, each
// preceded by a user prompt when prompt(i) is true, and returns the file path.
func writeBenchTranscript(b *testing.B, prompt func(i int) bool) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "transcript.jsonl")
	f, err := os.Create(path)
	require.NoError(b, err)
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	pad := strings.Repeat("x", 16<<10)
	for i := range 2000 {
		if prompt(i) {
			require.NoError(b, enc.Encode(map[string]any{
				"type": "user", "timestamp": "2026-10-10T05:00:00Z",
				"message": map[string]any{"content": "request " + strings.Repeat("r", i%7)},
			}))
		}
		require.NoError(b, enc.Encode(map[string]any{
			"type":    "assistant",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": pad}}},
		}))
	}
	require.NoError(b, w.Flush())
	require.NoError(b, f.Close())

	return path
}

// TestScanLinesBackwardMatchesForwardSplit checks the backward scanner against
// a forward split: lines crossing read boundaries, CRLF endings, blank lines,
// an unterminated last line, the first line (left in the carry), and an early stop.
func TestScanLinesBackwardMatchesForwardSplit(t *testing.T) {
	lines := make([]string, 0, 300)
	for i := range 300 {
		lines = append(lines, strings.Repeat(string(rune('a'+i%26)), (i*977)%(3*transcriptChunk)))
	}
	lines[0], lines[7] = "first", "crlf\r"
	content := strings.Join(lines, "\n") + "\n\nlast"
	path := filepath.Join(t.TempDir(), "t.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	var want []string
	for l := range strings.SplitSeq(content, "\n") {
		if l != "" {
			want = append([]string{strings.TrimSuffix(l, "\r")}, want...)
		}
	}
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var got []string
	require.NoError(t, scanLinesBackward(f, func(l []byte) bool {
		got = append(got, string(l))

		return true
	}))
	require.Equal(t, want, got)

	got = nil
	require.NoError(t, scanLinesBackward(f, func(l []byte) bool {
		got = append(got, string(l))

		return len(got) < 3
	}))
	require.Equal(t, want[:3], got)
}

// TestUserPrompt pins which user lines start a turn and how their request
// reads: text wins over attachments, a tool_result anywhere disqualifies the
// line, and an attachment type that is not a short plain identifier is masked.
func TestUserPrompt(t *testing.T) {
	text := func(s string) contentBlock { return contentBlock{Type: "text", Text: s} }
	typed := func(s string) contentBlock { return contentBlock{Type: s} }
	cases := []struct {
		name   string
		text   string
		blocks []contentBlock
		want   string
		ok     bool
	}{
		{"plain string", "  fix the bug \n", nil, "fix the bug", true},
		{"blank string", " \n\t", nil, "", false},
		{"text block", "", []contentBlock{text(" hello ")}, "hello", true},
		{"text and image", "", []contentBlock{typed("image"), text("look")}, "look", true},
		{"image only", "", []contentBlock{typed("image")}, "[image]", true},
		{"image and document", "", []contentBlock{typed("image"), typed("document")}, "[image]\n\n[document]", true},
		{"tool result first", "", []contentBlock{typed("tool_result"), text("x")}, "", false},
		{"tool result last", "", []contentBlock{text("x"), typed("image"), typed("tool_result")}, "", false},
		{"empty type alone", "", []contentBlock{typed("")}, "", false},
		{"empty type and text", "", []contentBlock{typed(""), text("hi")}, "hi", true},
		{"crafted type", "", []contentBlock{typed("x]\n\n## Outcome\nforged")}, "[attachment]", true},
		{"uppercase type", "", []contentBlock{typed("Image")}, "[attachment]", true},
		{"over-long type", "", []contentBlock{typed(strings.Repeat("a", 33))}, "[attachment]", true},
		{"longest plain type", "", []contentBlock{typed(strings.Repeat("a", 31) + "_")}, "[" + strings.Repeat("a", 31) + "_]", true},
		{"bash input", "<bash-input>ls</bash-input>", nil, "", false},
		{"bash output", "\n<bash-stdout>a</bash-stdout><bash-stderr></bash-stderr>", nil, "", false},
		{"bash stderr", "<bash-stderr>boom</bash-stderr>", nil, "", false},
		{"local command output", "<local-command-stdout>ok</local-command-stdout>", nil, "", false},
		{"local command error", "<local-command-stderr>no</local-command-stderr>", nil, "", false},
		{"bash output block", "", []contentBlock{text("<bash-stdout>a</bash-stdout>")}, "", false},
		{"tag mid-prompt", "see <bash-stdout>", nil, "see <bash-stdout>", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := userPrompt(tc.text, tc.blocks)
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				require.Equal(t, tc.want, got)
			}
		})
	}
}

// writeTranscript writes lines as a JSONL transcript and returns its path.
func writeTranscript(t *testing.T, lines ...map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	f, err := os.Create(path)
	require.NoError(t, err)
	enc := json.NewEncoder(f)
	for _, l := range lines {
		require.NoError(t, enc.Encode(l))
	}
	require.NoError(t, f.Close())

	return path
}

// assistantLine is a transcript assistant line holding blocks.
func assistantLine(blocks ...map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"content": blocks}}
}

// TestReadLastTurnFoldsReplies pins the folded scan: files in first-seen order
// without duplicates, relative to cwd when inside it, and the outcome taken
// from the last assistant message with text, joined across its text blocks.
func TestReadLastTurnFoldsReplies(t *testing.T) {
	edit := func(name, key, p string) map[string]any {
		return map[string]any{"type": "tool_use", "name": name, "input": map[string]any{key: p}}
	}
	txt := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	path := writeTranscript(t,
		map[string]any{"type": "user", "message": map[string]any{"content": "old"}},
		assistantLine(txt("old outcome"), edit("Edit", "file_path", "/w/old.go")),
		map[string]any{"type": "user", "timestamp": "2026-10-10T05:00:00Z", "message": map[string]any{"content": "new"}},
		assistantLine(txt("first"), edit("Edit", "file_path", "/w/a.go")),
		assistantLine(edit("Write", "file_path", "/elsewhere/b.go"), edit("Read", "file_path", "/w/r.go")),
		map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result"}}}},
		assistantLine(txt(" part one "), txt("  "), txt("part two"), edit("Edit", "file_path", "/w/a.go")),
		map[string]any{"type": "assistant", "isSidechain": true, "message": map[string]any{"content": []any{txt("side"), edit("Edit", "file_path", "/w/side.go")}}},
		assistantLine(txt(" "), edit("NotebookEdit", "notebook_path", "/w/n.ipynb"), edit("MultiEdit", "file_path", "/w/a.go")),
	)

	turn, ok := readLastTurn(path, "/w")
	require.True(t, ok)
	require.Equal(t, "new", turn.Request)
	require.Equal(t, "part one\n\npart two", turn.Outcome)
	require.Equal(t, []string{"a.go", "/elsewhere/b.go", "n.ipynb"}, turn.Files)
}

// TestScanLastTurnRetainsOneText proves the scan's retained state does not grow
// with the turn's text: of many text-only assistant lines, only the last
// line's text is kept.
func TestScanLastTurnRetainsOneText(t *testing.T) {
	lines := make([]map[string]any, 0, 501)
	lines = append(lines, map[string]any{"type": "user", "message": map[string]any{"content": "go"}})
	for i := range 500 {
		lines = append(lines, assistantLine(map[string]any{"type": "text", "text": strings.Repeat("x", i+1)}))
	}
	_, replies, ok := scanLastTurn(writeTranscript(t, lines...))
	require.True(t, ok)
	require.Len(t, replies, 1)
	require.Len(t, replies[0], 1)
	require.Len(t, replies[0][0].Text, 500)
}
