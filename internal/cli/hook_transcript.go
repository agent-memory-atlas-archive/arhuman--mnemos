package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/arhuman/mnemos/internal/app"
	"github.com/arhuman/mnemos/internal/memory"
	"github.com/arhuman/mnemos/internal/security"
)

// transcriptMaxLine is the longest transcript line the scanner accepts. A tool
// result can embed a whole file, so the default 64 KiB is far too small.
const transcriptMaxLine = 16 << 20

// newHookStopCmd builds `hook stop`: the Stop hook journal writer. It condenses
// the turn that just ended (request, files touched, final assistant text) into
// one journal entry (ADR-0011) and prints nothing, whatever the outcome.
func newHookStopCmd(state *rootState) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Write one journal entry per turn for a Claude Code Stop hook",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hookStop(cmd, state)

			return nil
		},
	}
}

// hookStop writes the journal entry for the turn described by the Stop payload
// on stdin. Every failure (bad payload, re-entrant stop, missing transcript, no
// workspace, write gate off, secret found, ingest error) is swallowed: the
// entry is dropped and the session is never affected.
func hookStop(cmd *cobra.Command, state *rootState) {
	raw, ok := readHookStdin(cmd)
	if !ok {
		return
	}
	var in hookInput
	if json.Unmarshal(raw, &in) != nil || in.StopHookActive || in.TranscriptPath == "" {
		return
	}
	turn, ok := readLastTurn(in.TranscriptPath, in.Cwd)
	if !ok {
		return
	}

	_ = withStore(state, false, func(a *app.App) error {
		svc := memory.New(a.DB, a.Config, a.TreeRoot(), security.NewRegexScanner(), a.Logger)
		turn.SessionID = in.SessionID
		_, rerr := svc.WriteJournalEntry(ctx(cmd), turn)
		if rerr != nil {
			a.Logger.Debug("hook stop: journal entry dropped", "err", rerr)
		}

		return nil
	})
}

// transcriptLine is the subset of a Claude Code transcript JSONL line the Stop
// hook reads.
type transcriptLine struct {
	Type        string            `json:"type"`
	IsSidechain bool              `json:"isSidechain"`
	IsMeta      bool              `json:"isMeta"`
	Timestamp   string            `json:"timestamp"`
	Message     transcriptMessage `json:"message"`
}

// transcriptMessage carries a line's content: a string or an array of blocks.
type transcriptMessage struct {
	Content json.RawMessage `json:"content"`
}

// contentBlock is one element of a message's content array.
type contentBlock struct {
	Type  string    `json:"type"`
	Text  string    `json:"text"`
	Name  string    `json:"name"`
	Input toolInput `json:"input"`
}

// toolInput is the subset of a tool_use input naming the file a tool edits.
type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

// readLastTurn parses the transcript at path and returns the last turn, or false
// when the file is unreadable, a line exceeds transcriptMaxLine (the turn in
// hand would be a stale one), or it holds no real user prompt. Malformed lines
// and sidechain (subagent) lines are skipped. File paths are rendered relative
// to cwd when they sit inside it.
func readLastTurn(path, cwd string) (memory.JournalTurn, bool) {
	f, err := os.Open(path) //nolint:gosec // the transcript path comes from the harness payload by design
	if err != nil {
		return memory.JournalTurn{}, false
	}
	defer func() { _ = f.Close() }()

	var (
		turn  memory.JournalTurn
		found bool
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), transcriptMaxLine)
	for sc.Scan() {
		var line transcriptLine
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.IsSidechain {
			continue
		}
		text, blocks := decodeContent(line.Message.Content)
		switch {
		case line.Type == "user" && !line.IsMeta:
			if prompt, ok := userPrompt(text, blocks); ok {
				turn = memory.JournalTurn{Request: prompt, At: parseTimestamp(line.Timestamp)}
				found = true
			}
		case line.Type == "assistant" && found:
			addAssistant(&turn, blocks, cwd)
		default:
		}
	}
	if sc.Err() != nil || !found {
		return memory.JournalTurn{}, false
	}

	return turn, true
}

// addAssistant folds one assistant message into the turn: edited files are
// appended once each in first-seen order, and a message with text replaces the
// outcome, so the turn ends up holding the last assistant text.
func addAssistant(t *memory.JournalTurn, blocks []contentBlock, cwd string) {
	var texts []string
	for _, b := range blocks {
		if txt := strings.TrimSpace(b.Text); b.Type == "text" && txt != "" {
			texts = append(texts, txt)
		}
		if p := touchedPath(b, cwd); b.Type == "tool_use" && p != "" && !slices.Contains(t.Files, p) {
			t.Files = append(t.Files, p)
		}
	}
	if len(texts) > 0 {
		t.Outcome = strings.Join(texts, "\n\n")
	}
}

// decodeContent splits a message content field into its string form or its
// block array. Content of neither shape yields "" and no blocks.
func decodeContent(raw json.RawMessage) (string, []contentBlock) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return "", blocks
	}

	return "", nil
}

// userPrompt returns the prompt text of a user line that is a real prompt: a
// non-empty string, or blocks with at least one text block and no tool_result
// (a tool result travels as a user line but is not something the user typed).
func userPrompt(text string, blocks []contentBlock) (string, bool) {
	if blocks == nil {
		text = strings.TrimSpace(text)

		return text, text != ""
	}
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			return "", false
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				texts = append(texts, t)
			}
		default:
		}
	}

	return strings.Join(texts, "\n\n"), len(texts) > 0
}

// touchedPath returns the file a tool_use block edited, relative to cwd when
// inside it, or "" for a tool that does not edit files.
func touchedPath(b contentBlock, cwd string) string {
	var p string
	switch b.Name {
	case "Edit", "Write", "MultiEdit":
		p = b.Input.FilePath
	case "NotebookEdit":
		p = b.Input.NotebookPath
	default:
		return ""
	}
	if p == "" || cwd == "" {
		return p
	}
	rel, err := filepath.Rel(cwd, p)
	if err == nil && filepath.IsAbs(p) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}

	return p
}

// parseTimestamp reads a transcript RFC3339 timestamp as UTC, falling back to
// now so an entry is never dropped for a missing or malformed stamp.
func parseTimestamp(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}

	return time.Now().UTC()
}
