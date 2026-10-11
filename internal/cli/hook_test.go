package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arhuman/mnemos/internal/cli"
	"github.com/arhuman/mnemos/internal/storage"
)

// runHook executes the root command with args, feeding stdin, and captures
// stdout. Hook subcommands read the Claude Code event JSON from stdin, so the
// plain runCmd helper (no stdin) cannot exercise recall.
func runHook(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	root := cli.NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	require.NoError(t, root.Execute())

	return out.String()
}

// seedWorkingSet indexes two open tasks (in_progress, todo), one done task, and
// one decision document in a fresh local workspace under the current temp dir.
func seedWorkingSet(t *testing.T) {
	t.Helper()
	runCmd(t, "init")
	seedKB(t, "tasks/live.md", "---\ntype: Task\nstatus: in_progress\ntitle: Wire the gateway\n---\n# Wire the gateway\n\nbody\n")
	seedKB(t, "tasks/next.md", "---\ntype: Task\nstatus: todo\ntitle: Add CSV export\n---\n# Add CSV export\n\nbody\n")
	seedKB(t, "tasks/old.md", "---\ntype: Task\nstatus: done\ntitle: Ancient history\n---\n# Ancient history\n\nbody\n")
	seedKB(t, "decisions/0001-anchor.md", "---\ntype: decision\ntitle: Single MNEMOS_DIR anchor\n---\n# Single MNEMOS_DIR anchor\n\nbody\n")
	runCmd(t, "ingest", ".", "--collection", "demo")
}

func TestHookSessionStartInjectsScopedWorkingSet(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "") // force cwd resolution to the local workspace
	seedWorkingSet(t)

	out := runHook(t, "", "hook", "session-start")

	require.Contains(t, out, "## mnemos working set")
	require.Contains(t, out, "### Open tasks")
	require.Contains(t, out, "Wire the gateway")
	require.Contains(t, out, "Add CSV export")
	require.Contains(t, out, "### Recent decisions")
	require.Contains(t, out, "Single MNEMOS_DIR anchor")
	// done tasks are never in the working set.
	require.NotContains(t, out, "Ancient history")
	// project scope: no per-collection labelling banner.
	require.NotContains(t, out, "labelled by collection")
}

func TestHookSessionStartSilentWithoutWorkspace(t *testing.T) {
	// A signalled project with no .mnemos resolves fail-closed to an uninitialized
	// location; the absent database must make the hook stay silent, never dump the
	// global store.
	proj := t.TempDir()
	t.Setenv("CLAUDE_PROJECT_DIR", proj)
	chdir(t, t.TempDir())

	out := runHook(t, "", "hook", "session-start")

	require.Empty(t, out)
}

func TestHookRecallInjectsHits(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	runCmd(t, "init")
	seedKB(t, "decisions/0002-staging.md",
		"---\ntype: decision\ntitle: Staging on the VPS\n---\n# Staging on the VPS\n\nWe choose staging on the vps because it is cheap.\n")
	runCmd(t, "ingest", ".", "--collection", "demo")

	out := runHook(t, `{"prompt":"why did we choose staging on the vps?","hook_event_name":"UserPromptSubmit"}`, "hook", "recall")

	require.Contains(t, out, "## mnemos recall")
	require.Contains(t, out, "decisions/0002-staging.md")
}

func TestHookRecallSilentOnNoHits(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	runCmd(t, "init")
	seedKB(t, "decisions/0002-staging.md", "---\ntype: decision\ntitle: Staging\n---\n# Staging\n\nWe choose staging on the vps.\n")
	runCmd(t, "ingest", ".", "--collection", "demo")

	out := runHook(t, `{"prompt":"what about banana pancakes for breakfast?"}`, "hook", "recall")

	require.Empty(t, out)
}

func TestHookRecallSilentOnNonQuestion(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	runCmd(t, "init")
	seedKB(t, "decisions/0002-staging.md", "---\ntype: decision\ntitle: Staging\n---\n# Staging\n\nstaging vps notes\n")
	runCmd(t, "ingest", ".", "--collection", "demo")

	// No cue phrase and not question-shaped: recall must not fire even though the
	// terms would match.
	out := runHook(t, `{"prompt":"update the staging vps config"}`, "hook", "recall")

	require.Empty(t, out)
}

func TestHookRecallSilentWithoutWorkspace(t *testing.T) {
	proj := t.TempDir()
	t.Setenv("CLAUDE_PROJECT_DIR", proj)
	chdir(t, t.TempDir())

	out := runHook(t, `{"prompt":"why did we choose staging on the vps?"}`, "hook", "recall")

	require.Empty(t, out)
}

// stopCwd is the project directory the Stop payloads claim; edited paths under
// it render relative in the journal entry.
const stopCwd = "/work/proj"

// writeTranscript writes lines as a JSONL transcript in a temp dir and returns
// its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	return p
}

// stopPayload renders a Stop hook payload for the given transcript.
func stopPayload(t *testing.T, transcript string, active bool) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"hook_event_name":  "Stop",
		"session_id":       "abcdef12-3456-7890",
		"transcript_path":  transcript,
		"cwd":              stopCwd,
		"stop_hook_active": active,
	})
	require.NoError(t, err)

	return string(raw)
}

// journalEntries lists the entry files under the local kb journal directory,
// ignoring the per-directory log.md.
func journalEntries(t *testing.T) []string {
	t.Helper()
	var out []string
	root := kbPath("journal")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") && d.Name() != "log.md" {
			rel, _ := filepath.Rel(filepath.Join(".mnemos", "kb"), p)
			out = append(out, filepath.ToSlash(rel))
		}

		return nil
	})

	return out
}

// userLine, toolResultLine and assistantLine build transcript lines of the
// shapes the Stop hook reads.
func userLine(ts, text string) string {
	raw, _ := json.Marshal(map[string]any{
		"type": "user", "isSidechain": false, "isMeta": false, "timestamp": ts,
		"message": map[string]any{"role": "user", "content": text},
	})

	return string(raw)
}

func toolResultLine(ts string) string {
	raw, _ := json.Marshal(map[string]any{
		"type": "user", "isSidechain": false, "timestamp": ts,
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
		}},
	})

	return string(raw)
}

func assistantLine(sidechain bool, blocks ...map[string]any) string {
	raw, _ := json.Marshal(map[string]any{
		"type": "assistant", "isSidechain": sidechain,
		"message": map[string]any{"role": "assistant", "content": blocks},
	})

	return string(raw)
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func editBlock(name, key, path string) map[string]any {
	return map[string]any{"type": "tool_use", "name": name, "input": map[string]any{key: path}}
}

// stopWorkspace initializes a local workspace with allow_write on.
func stopWorkspace(t *testing.T) {
	t.Helper()
	chdir(t, t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	runCmd(t, "init")
	enableWrite(t)
}

func TestHookStopWritesOneJournalEntry(t *testing.T) {
	stopWorkspace(t)
	transcript := writeTranscript(t,
		userLine("2026-10-10T05:31:42.123Z", "Add the stop hook: title line\nwith details"),
		assistantLine(false, textBlock("Looking."), editBlock("Edit", "file_path", stopCwd+"/internal/cli/hook.go")),
		toolResultLine("2026-10-10T05:31:50Z"),
		assistantLine(false,
			editBlock("Write", "file_path", "/elsewhere/notes.md"),
			editBlock("MultiEdit", "file_path", stopCwd+"/internal/cli/hook.go"),
			editBlock("NotebookEdit", "notebook_path", stopCwd+"/nb.ipynb"),
			editBlock("Read", "file_path", stopCwd+"/ignored.go")),
		toolResultLine("2026-10-10T05:32:00Z"),
		assistantLine(false, textBlock("Done: the hook writes one entry.")),
	)

	out := runHook(t, stopPayload(t, transcript, false), "hook", "stop")

	require.Empty(t, out)
	require.Equal(t, []string{"journal/2026-10-10/abcdef12-053142.md"}, journalEntries(t))
	raw, err := os.ReadFile(kbPath("journal/2026-10-10/abcdef12-053142.md"))
	require.NoError(t, err)
	body := string(raw)
	require.True(t, strings.HasPrefix(body, "---\ntype: journal\n"), body)
	require.Contains(t, body, "title: 'Add the stop hook: title line'")
	require.Contains(t, body, "captured_at: \"2026-10-10T05:31:42Z\"")
	require.Contains(t, body, "session_id: abcdef12-3456-7890")
	require.Contains(t, body, "## Request\n\nAdd the stop hook: title line\nwith details\n")
	require.Contains(t, body, "## Files touched\n\n- internal/cli/hook.go\n- /elsewhere/notes.md\n- nb.ipynb\n\n")
	require.NotContains(t, body, "ignored.go")
	require.Contains(t, body, "## Outcome\n\nDone: the hook writes one entry.\n")
	require.NotContains(t, body, "Looking.")

	db, err := sql.Open("sqlite", filepath.Join(".mnemos", "state", "index.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	journal, err := storage.IsJournalURI(context.Background(), db, "journal/2026-10-10/abcdef12-053142.md")
	require.NoError(t, err)
	require.True(t, journal)
}

func TestHookStopCapturesOnlyLastTurn(t *testing.T) {
	stopWorkspace(t)
	transcript := writeTranscript(t,
		userLine("2026-10-10T05:00:00Z", "first request"),
		assistantLine(false, textBlock("first outcome"), editBlock("Edit", "file_path", stopCwd+"/first.go")),
		userLine("2026-10-10T06:00:00Z", "second request"),
		assistantLine(false, editBlock("Edit", "file_path", stopCwd+"/second.go")),
		toolResultLine("2026-10-10T06:00:05Z"),
		"{not json",
		assistantLine(true, textBlock("sidechain chatter"), editBlock("Edit", "file_path", stopCwd+"/sub.go")),
		`{"type":"user","isSidechain":true,"timestamp":"2026-10-10T07:00:00Z","message":{"role":"user","content":"subagent prompt"}}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-10-10T07:00:00Z","message":{"role":"user","content":"meta caveat"}}`,
		assistantLine(false, textBlock("second outcome")),
	)

	out := runHook(t, stopPayload(t, transcript, false), "hook", "stop")

	require.Empty(t, out)
	entries := journalEntries(t)
	require.Equal(t, []string{"journal/2026-10-10/abcdef12-060000.md"}, entries)
	raw, err := os.ReadFile(kbPath(entries[0]))
	require.NoError(t, err)
	body := string(raw)
	require.Contains(t, body, "second request")
	require.Contains(t, body, "- second.go\n")
	require.Contains(t, body, "second outcome")
	for _, absent := range []string{"first request", "first outcome", "first.go", "sidechain chatter", "sub.go", "subagent prompt", "meta caveat"} {
		require.NotContains(t, body, absent)
	}
}

func TestHookStopRerunIsIdempotent(t *testing.T) {
	stopWorkspace(t)
	transcript := writeTranscript(t,
		userLine("2026-10-10T05:31:42Z", "do the thing"),
		assistantLine(false, textBlock("did the thing")),
	)

	runHook(t, stopPayload(t, transcript, false), "hook", "stop")
	runHook(t, stopPayload(t, transcript, false), "hook", "stop")

	require.Equal(t, []string{"journal/2026-10-10/abcdef12-053142.md"}, journalEntries(t))
}

func TestHookStopSilentWhenWriteDisabled(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	runCmd(t, "init")
	transcript := writeTranscript(t,
		userLine("2026-10-10T05:31:42Z", "do the thing"),
		assistantLine(false, textBlock("did the thing")),
	)

	out := runHook(t, stopPayload(t, transcript, false), "hook", "stop")

	require.Empty(t, out)
	require.Empty(t, journalEntries(t))
}

func TestHookStopDropsEntryWithSecret(t *testing.T) {
	stopWorkspace(t)
	transcript := writeTranscript(t,
		userLine("2026-10-10T05:31:42Z", "deploy with AKIAQYLPMN5HXYZ12345 please"),
		assistantLine(false, textBlock("deployed")),
	)

	out := runHook(t, stopPayload(t, transcript, false), "hook", "stop")

	require.Empty(t, out)
	require.Empty(t, journalEntries(t))
}

func TestHookStopSilentNoOps(t *testing.T) {
	transcript := func(t *testing.T) string {
		t.Helper()

		return writeTranscript(t,
			userLine("2026-10-10T05:31:42Z", "do the thing"),
			assistantLine(false, textBlock("did the thing")),
		)
	}
	cases := map[string]func(t *testing.T) string{
		"stop_hook_active": func(t *testing.T) string { return stopPayload(t, transcript(t), true) },
		"malformed stdin":  func(*testing.T) string { return "{not json" },
		"missing transcript": func(t *testing.T) string {
			return stopPayload(t, filepath.Join(t.TempDir(), "absent.jsonl"), false)
		},
		"no user prompt": func(t *testing.T) string {
			return stopPayload(t, writeTranscript(t, toolResultLine("2026-10-10T05:31:42Z"), assistantLine(false, textBlock("x"))), false)
		},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			stopWorkspace(t)

			out := runHook(t, payload(t), "hook", "stop")

			require.Empty(t, out)
			require.Empty(t, journalEntries(t))
		})
	}

	t.Run("no workspace", func(t *testing.T) {
		proj := t.TempDir()
		t.Setenv("CLAUDE_PROJECT_DIR", proj)
		chdir(t, t.TempDir())

		out := runHook(t, stopPayload(t, transcript(t), false), "hook", "stop")

		require.Empty(t, out)
		require.NoDirExists(t, filepath.Join(proj, ".mnemos"))
		require.NoDirExists(t, ".mnemos")
	})
}

// TestHookStopKeepsPromotedEntry reproduces P19: a second Stop on the same last
// prompt must not rewrite a promoted entry back into the pending list.
func TestHookStopKeepsPromotedEntry(t *testing.T) {
	stopWorkspace(t)
	transcript := writeTranscript(t,
		userLine("2026-10-10T05:31:42Z", "do the thing"),
		assistantLine(false, textBlock("did the thing")),
	)
	const entry = "journal/2026-10-10/abcdef12-053142.md"

	runHook(t, stopPayload(t, transcript, false), "hook", "stop")
	runCmd(t, "journal", "promote", entry, "none")
	runHook(t, stopPayload(t, transcript, false), "hook", "stop")

	raw, err := os.ReadFile(kbPath(entry))
	require.NoError(t, err)
	require.Contains(t, string(raw), "promoted_to: none")
	require.Equal(t, "no pending journal entries\n", runCmd(t, "journal", "pending"))
}

// TestHookStopScanErrorDropsEntry reproduces P23: a line past the cap inside
// the last turn must drop the entry, never journal a partial or earlier turn. A
// long line before the last prompt is never read (P30), so that turn is kept.
func TestHookStopScanErrorDropsEntry(t *testing.T) {
	long := assistantLine(false, textBlock(strings.Repeat("x", 17<<20)))

	t.Run("inside the last turn", func(t *testing.T) {
		stopWorkspace(t)
		transcript := writeTranscript(t,
			userLine("2026-10-10T05:00:00Z", "first request"),
			assistantLine(false, textBlock("first outcome")),
			userLine("2026-10-10T06:00:00Z", "second request"),
			long,
			assistantLine(false, textBlock("second outcome")),
		)

		require.Empty(t, runHook(t, stopPayload(t, transcript, false), "hook", "stop"))
		require.Empty(t, journalEntries(t))
	})

	t.Run("before the last prompt", func(t *testing.T) {
		stopWorkspace(t)
		transcript := writeTranscript(t,
			userLine("2026-10-10T05:00:00Z", "first request"),
			long,
			userLine("2026-10-10T06:00:00Z", "second request"),
			assistantLine(false, textBlock("second outcome")),
		)

		require.Empty(t, runHook(t, stopPayload(t, transcript, false), "hook", "stop"))
		require.Equal(t, []string{"journal/2026-10-10/abcdef12-060000.md"}, journalEntries(t))
		body, err := os.ReadFile(kbPath("journal/2026-10-10/abcdef12-060000.md"))
		require.NoError(t, err)
		require.Contains(t, string(body), "## Outcome\n\nsecond outcome\n")
	})
}

// TestHookStdinLimit pins that an oversized hook payload is ignored rather than
// read whole: a valid payload padded past the stdin cap (1 MiB) stays silent.
func TestHookStdinLimit(t *testing.T) {
	padding := strings.Repeat(" ", 2<<20)

	t.Run("recall", func(t *testing.T) {
		chdir(t, t.TempDir())
		t.Setenv("CLAUDE_PROJECT_DIR", "")
		runCmd(t, "init")
		seedKB(t, "decisions/0002-staging.md",
			"---\ntype: decision\ntitle: Staging on the VPS\n---\n# Staging on the VPS\n\nWe choose staging on the vps because it is cheap.\n")
		runCmd(t, "ingest", ".", "--collection", "demo")
		payload := `{"prompt":"why did we choose staging on the vps?"}`

		require.Contains(t, runHook(t, payload, "hook", "recall"), "## mnemos recall")
		require.Empty(t, runHook(t, payload+padding, "hook", "recall"))
	})

	t.Run("stop", func(t *testing.T) {
		stopWorkspace(t)
		transcript := writeTranscript(t,
			userLine("2026-10-10T05:31:42Z", "do the thing"),
			assistantLine(false, textBlock("did the thing")),
		)

		runHook(t, stopPayload(t, transcript, false)+padding, "hook", "stop")

		require.Empty(t, journalEntries(t))
	})
}
