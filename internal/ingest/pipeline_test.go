package ingest_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arhuman/mnemos/internal/chunk"
	"github.com/arhuman/mnemos/internal/ingest"
	"github.com/arhuman/mnemos/internal/security"
	"github.com/arhuman/mnemos/internal/storage"
	"github.com/arhuman/mnemos/internal/testutil"
)

// newDB returns a fresh migrated database for pipeline tests.
func newDB(t *testing.T) *sql.DB { return testutil.NewDB(t) }

// write creates a file with content under dir, making parent directories.
func write(t *testing.T, dir, rel, content string) { testutil.WriteFile(t, dir, rel, content) }

func TestPipelineRun(t *testing.T) {
	src := t.TempDir()
	write(t, src, "a.md", "# Title\n\nBody [l](b.md)\n")
	write(t, src, "index.md", "# Bundle\n\n[hub](a.md)\n")
	write(t, src, "n.txt", "plain\n")

	db := newDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := ingest.New(db, logger)

	opts := ingest.Options{
		Root:       src,
		Collection: "c",
		Rules: ingest.Rules{
			Include: []string{"**/*.md", "**/*.txt"},
		},
		Chunking: chunk.Config{TargetTokens: 700, OverlapTokens: 80},
	}

	sum, err := p.Run(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, 3, sum.FilesScanned)
	require.Equal(t, 3, sum.FilesIngested)
	require.Equal(t, 0, sum.FilesSkipped)
	require.Positive(t, sum.ChunksWritten)

	// Second run skips every unchanged file.
	sum2, err := p.Run(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, 0, sum2.FilesIngested)
	require.Equal(t, 3, sum2.FilesSkipped)
	require.Equal(t, 0, sum2.ChunksWritten)

	// Mutating a file makes it ingest again (and only it).
	write(t, src, "a.md", "# Title\n\nChanged body\n")
	sum3, err := p.Run(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, sum3.FilesIngested)
	require.Equal(t, 2, sum3.FilesSkipped)
}

func TestPipelineSkipsOversizeFile(t *testing.T) {
	src := t.TempDir()
	write(t, src, "small.md", "# Small\n\nbody\n")
	write(t, src, "big.md", "# Big\n\n"+strings.Repeat("x", 4096))

	db := newDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Cap below big.md but above small.md: the big file is skipped, not read.
	p := ingest.New(db, logger, ingest.WithMaxFileBytes(1024))

	opts := ingest.Options{
		Root:       src,
		Collection: "c",
		Rules:      ingest.Rules{Include: []string{"**/*.md"}},
		Chunking:   chunk.Config{TargetTokens: 700, OverlapTokens: 80},
	}

	sum, err := p.Run(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, 2, sum.FilesScanned)
	require.Equal(t, 1, sum.FilesIngested, "only the small file is ingested")
	require.Equal(t, 1, sum.FilesSkipped, "the oversize file is skipped")

	// With the cap disabled (0), the same big file is ingested.
	pNoCap := ingest.New(db, logger, ingest.WithMaxFileBytes(0))
	sum2, err := pNoCap.Run(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, 1, sum2.FilesIngested, "big file now ingests; small file is unchanged")
}

func TestPipelineEmptyRoot(t *testing.T) {
	src := t.TempDir()
	db := newDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sum, err := ingest.New(db, logger).Run(context.Background(), ingest.Options{
		Root:     src,
		Rules:    ingest.Rules{Include: []string{"**/*.md"}},
		Chunking: chunk.Config{TargetTokens: 700, OverlapTokens: 80},
	})
	require.NoError(t, err)
	require.Equal(t, 0, sum.FilesScanned)
	require.Equal(t, 0, sum.FilesIngested)
}

// TestRunSkipsFileHoldingSecrets covers the ingest secret screen. The index is
// served to an LLM, so a credential that reaches it is disclosed on the next
// matching search; screening at ingest holds the bulk path to the same bar as
// remember/okfy. The clean file in the same run must still index, since one
// skipped file must not abort the batch.
func TestRunSkipsFileHoldingSecrets(t *testing.T) {
	db := newDB(t)
	src := t.TempDir()
	write(t, src, "clean.md", "# Clean\n\nnothing sensitive here\n")
	write(t, src, "leak.md", "# Runbook\n\ndeploy with AKIAQYLPMN5HXYZ12345 in the env\n")

	p := ingest.New(db, slog.New(slog.NewTextHandler(io.Discard, nil)),
		ingest.WithSecretScanner(security.NewRegexScanner()))
	summary, err := p.Run(context.Background(), ingest.Options{
		Root:       src,
		Collection: "c",
		Rules:      ingest.Rules{Include: []string{"**/*.md"}},
		Chunking:   chunk.Config{TargetTokens: 700, OverlapTokens: 80},
	})
	require.NoError(t, err)
	require.Equal(t, 1, summary.FilesIngested, "only the clean file should be ingested")

	leaked, err := storage.GetChunksByDocURI(context.Background(), db, "leak.md")
	require.NoError(t, err)
	require.Empty(t, leaked, "a file holding a secret must not be indexed")

	clean, err := storage.GetChunksByDocURI(context.Background(), db, "clean.md")
	require.NoError(t, err)
	require.NotEmpty(t, clean, "a skip must not abort the rest of the batch")
}

// TestRunWithoutScannerIndexesEverything pins the default: no scanner configured
// means no screening, so adding the option cannot silently change behavior for a
// caller that did not ask for it.
func TestRunWithoutScannerIndexesEverything(t *testing.T) {
	db := newDB(t)
	src := t.TempDir()
	write(t, src, "leak.md", "# Runbook\n\ndeploy with AKIAQYLPMN5HXYZ12345 in the env\n")

	summary, err := ingest.New(db, slog.New(slog.NewTextHandler(io.Discard, nil))).
		Run(context.Background(), ingest.Options{
			Root:       src,
			Collection: "c",
			Rules:      ingest.Rules{Include: []string{"**/*.md"}},
			Chunking:   chunk.Config{TargetTokens: 700, OverlapTokens: 80},
		})
	require.NoError(t, err)
	require.Equal(t, 1, summary.FilesIngested, "no scanner means no screening")
}

// TestIngestAcceptedPlaceholder is the GH-49 case end to end: a documented
// all-zero placeholder is indexed when a matching [[security.accept]] entry
// covers it, skipped without one, and still skipped when the same file also
// holds a real credential, since an entry drops one finding, never the file.
func TestIngestAcceptedPlaceholder(t *testing.T) {
	const placeholder = "// Fixed offline fallback placeholder, not a credential.\n" +
		"private const string OfflineToken = \"00000000000000000000000000000000\";\n"
	accept := []security.Accept{{
		Rule:       "generic-credential-assignment",
		Identifier: "token",
		Value:      "00000000000000000000000000000000",
		Paths:      []string{"src/**/OfflineAuth.cs"},
		Reason:     "offline fallback placeholder",
	}}
	require.NoError(t, security.ValidateAccepts(accept))

	cases := []struct {
		name    string
		content string
		accepts []security.Accept
		indexed bool
	}{
		{"accepted placeholder is indexed", placeholder, accept, true},
		{"placeholder without an entry is skipped", placeholder, nil, false},
		{"another credential in the file still skips it", placeholder + "// AKIAQYLPMN5HXYZ12345\n", accept, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newDB(t)
			src := t.TempDir()
			write(t, src, "src/Auth/OfflineAuth.cs", tc.content)

			// Through the watcher, so its scanner wiring is what is under test: its
			// startup reconcile is a full ingest, done once Ready closes.
			w, err := ingest.NewWatcher(db, slog.New(slog.NewTextHandler(io.Discard, nil)), src, "c", ingest.WatchConfig{
				Include:       []string{"**/*.cs"},
				Chunking:      chunk.Config{TargetTokens: 700, OverlapTokens: 80},
				ScanSecrets:   true,
				SecretAccepts: tc.accepts,
			})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- w.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			select {
			case <-w.Ready():
			case runErr := <-done:
				t.Fatalf("watcher exited before ready: %v", runErr)
			case <-time.After(10 * time.Second):
				t.Fatal("watcher did not become ready")
			}

			chunks, err := storage.GetChunksByDocURI(context.Background(), db, "src/Auth/OfflineAuth.cs")
			require.NoError(t, err)
			if tc.indexed {
				require.NotEmpty(t, chunks)
			} else {
				require.Empty(t, chunks)
			}
		})
	}
}
