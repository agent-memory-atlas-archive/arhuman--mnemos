//go:build darwin || linux

package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// peakRSSEnv carries the transcript path to the child process of
// TestReadLastTurnPeakRSS; its presence makes the test run as that child.
const peakRSSEnv = "MNEMOS_PEAK_RSS_TRANSCRIPT"

// peakRSS returns the process's peak resident set in bytes. Linux reads VmHWM:
// its ru_maxrss carries the parent's peak across exec, which would hide the call.
func peakRSS(t *testing.T) int64 {
	t.Helper()
	if runtime.GOOS != "linux" {
		var ru syscall.Rusage
		require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &ru))

		return ru.Maxrss // bytes on darwin
	}
	status, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	for line := range strings.SplitSeq(string(status), "\n") {
		kb, found := strings.CutPrefix(line, "VmHWM:")
		if !found {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(kb, "kB")), 10, 64)
		require.NoError(t, err)

		return n << 10
	}
	require.FailNow(t, "no VmHWM in /proc/self/status")

	return 0
}

// TestReadLastTurnPeakRSS bounds how much one readLastTurn call raises the
// peak RSS on a 64 MiB text-only turn. A heap reading after the call returns
// cannot see a peak freed before return (raw lines pinned in a local slice
// during the scan), so the test re-executes its own binary and the child
// reports its peak RSS growth across the call alone. Measured growth, plain and
// under -race, when the scan keeps one line: darwin 5 and 14 MiB, linux 5 and
// 13 MiB; pinning the raw lines: darwin 97 and 307 MiB, linux 93 and 271 MiB.
// The 40 MiB bound sits between the two regimes on both.
func TestReadLastTurnPeakRSS(t *testing.T) {
	if path := os.Getenv(peakRSSEnv); path != "" {
		before := peakRSS(t)
		turn, ok := readLastTurn(path, "")
		require.True(t, ok)
		require.Len(t, turn.Outcome, 16<<10)
		growth := strconv.FormatInt(peakRSS(t)-before, 10)
		require.NoError(t, os.WriteFile(path+".rss", []byte(growth), 0o600))

		return
	}
	if testing.Short() {
		t.Skip("writes a 64 MiB transcript")
	}
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	f, err := os.Create(path)
	require.NoError(t, err)
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	require.NoError(t, enc.Encode(map[string]any{"type": "user", "message": map[string]any{"content": "go"}}))
	text := map[string]any{"type": "text", "text": strings.Repeat("x", 16<<10)}
	for range 4096 {
		require.NoError(t, enc.Encode(assistantLine(text)))
	}
	require.NoError(t, w.Flush())
	require.NoError(t, f.Close())

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReadLastTurnPeakRSS$", "-test.count=1")
	cmd.Env = append(os.Environ(), peakRSSEnv+"="+path)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "child: %s", out)
	raw, err := os.ReadFile(path + ".rss")
	require.NoError(t, err)
	growth, err := strconv.ParseInt(string(raw), 10, 64)
	require.NoError(t, err)
	t.Logf("peak RSS growth %d MiB", growth>>20)
	require.Less(t, growth, int64(40<<20), "readLastTurn raised peak RSS by %d bytes", growth)
}
