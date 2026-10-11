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

// BenchmarkReadLastTurn reads the last turn of a long session transcript: 2000
// turns of about 16 KiB each (32 MiB), the shape whose cost grows with the
// session when the whole file is parsed on every Stop.
func BenchmarkReadLastTurn(b *testing.B) {
	path := filepath.Join(b.TempDir(), "transcript.jsonl")
	f, err := os.Create(path)
	require.NoError(b, err)
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	pad := strings.Repeat("x", 16<<10)
	for i := range 2000 {
		require.NoError(b, enc.Encode(map[string]any{
			"type": "user", "timestamp": "2026-10-10T05:00:00Z",
			"message": map[string]any{"content": "request " + strings.Repeat("r", i%7)},
		}))
		require.NoError(b, enc.Encode(map[string]any{
			"type":    "assistant",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": pad}}},
		}))
	}
	require.NoError(b, w.Flush())
	require.NoError(b, f.Close())

	b.ResetTimer()
	for b.Loop() {
		if _, ok := readLastTurn(path, ""); !ok {
			b.Fatal("no turn read")
		}
	}
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
