package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/arhuman/mnemos/internal/app"
	"github.com/arhuman/mnemos/internal/memory"
)

// newJournalCmd builds the `journal` command: `pending` lists the session
// journal entries not yet reviewed, `promote` records where a reviewed entry's
// content went, and `prune` deletes old reviewed entries (ADR-0011). promote
// never writes the kb document; it stamps the entry after that document exists,
// and is gated by allow_write. prune is a dry run unless --apply, which is gated
// by allow_delete.
func newJournalCmd(state *rootState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "journal",
		Short: "Review session journal entries and record their promotion",
	}

	var limit int
	var asJSON bool
	pending := &cobra.Command{
		Use:   "pending",
		Short: "List journal entries not yet reviewed, oldest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJournalPending(cmd, state, limit, asJSON)
		},
	}
	pending.Flags().IntVar(&limit, "limit", 0, "maximum number of entries (0 = all)")
	pending.Flags().BoolVar(&asJSON, "json", false, "emit the entries as JSON")

	promote := &cobra.Command{
		Use:   "promote <entry-uri> <target>",
		Short: "Stamp a journal entry with the kb document now holding it, or none",
		Long: "Stamp a journal entry's promoted_to field with target: the uri of the kb document " +
			"already written from it, or \"" + memory.PromoteNone + "\" when nothing was worth promoting. It does not write " +
			"the kb document. Requires [mcp].allow_write=true.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJournalPromote(cmd, state, args[0], args[1])
		},
	}

	var olderThan string
	var apply, pruneJSON bool
	prune := &cobra.Command{
		Use:   "prune --older-than DURATION [--apply]",
		Short: "Delete reviewed journal entries older than a duration (dry run by default)",
		Long: "List, or with --apply delete, the journal entries already stamped with promoted_to " +
			"whose capture is older than --older-than (a Go duration such as 12h, or whole days such as 30d). " +
			"Unreviewed entries are never pruned. A dated directory left holding only its log.md is removed " +
			"with them; one holding an index.md is kept. --apply requires [mcp].allow_delete=true.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJournalPrune(cmd, state, olderThan, apply, pruneJSON)
		},
	}
	prune.Flags().StringVar(&olderThan, "older-than", "", "minimum age of a pruned entry: a Go duration (12h) or days (30d)")
	prune.Flags().BoolVar(&apply, "apply", false, "delete the entries instead of listing them")
	prune.Flags().BoolVar(&pruneJSON, "json", false, "emit the result as JSON")
	_ = prune.MarkFlagRequired("older-than")

	cmd.AddCommand(pending, promote, prune)

	return cmd
}

func runJournalPending(cmd *cobra.Command, state *rootState, limit int, asJSON bool) error {
	return withStore(state, false, func(a *app.App) error {
		svc := memory.New(a.DB, a.Config, a.TreeRoot(), nil, a.Logger)
		entries, err := svc.ListPendingJournal(cmd.Context(), limit)
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		if asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(entries); err != nil {
				return fmt.Errorf("journal: encode json: %w", err)
			}

			return nil
		}
		renderPendingJournal(out, entries)

		return nil
	})
}

// renderPendingJournal prints one aligned line per entry: uri, capture instant,
// title.
func renderPendingJournal(out io.Writer, entries []memory.JournalEntry) {
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(out, "no pending journal entries")

		return
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, e := range entries {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", e.URI, dash(e.CapturedAt), dash(e.Title))
	}
	_ = w.Flush()
}

func runJournalPromote(cmd *cobra.Command, state *rootState, entryURI, target string) error {
	return withStore(state, false, func(a *app.App) error {
		svc := memory.New(a.DB, a.Config, a.TreeRoot(), nil, a.Logger)
		res, err := svc.PromoteJournal(cmd.Context(), memory.PromoteJournalInput{EntryURI: entryURI, Target: target})
		if err != nil {
			return fmt.Errorf("journal promote: %w%s", err, promoteHint(err))
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s promoted_to %s\n", res.EntryURI, res.PromotedTo)

		return nil
	})
}

func runJournalPrune(cmd *cobra.Command, state *rootState, olderThan string, apply, asJSON bool) error {
	age, err := parseAge(olderThan)
	if err != nil {
		return fmt.Errorf("journal prune: %w", err)
	}

	return withStore(state, false, func(a *app.App) error {
		svc := memory.New(a.DB, a.Config, a.TreeRoot(), nil, a.Logger)
		res, err := svc.PruneJournal(cmd.Context(), memory.PruneJournalInput{OlderThan: age, Apply: apply})
		out := cmd.OutOrStdout()
		if err != nil {
			// A partial apply already deleted files: say which before failing.
			if len(res.Pruned) > 0 {
				renderPrunedJournal(out, res)
			}

			return err
		}
		if asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(res); err != nil {
				return fmt.Errorf("journal: encode json: %w", err)
			}

			return nil
		}
		renderPrunedJournal(out, res)

		return nil
	})
}

// parseAge reads a prune threshold: whole days as "<n>d", else any
// time.ParseDuration string. Positivity is PruneJournal's check.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --older-than %q: use a Go duration (12h) or days (30d)", s)
	}

	return d, nil
}

// renderPrunedJournal prints the pruned (or would-be pruned) count, one line per
// entry with its capture instant, the removed directories and the skip counts.
func renderPrunedJournal(out io.Writer, res memory.PruneJournalResult) {
	verb := "would prune"
	if res.Applied {
		verb = "pruned"
	}
	_, _ = fmt.Fprintf(out, "%s %d entries\n", verb, len(res.Pruned))
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, e := range res.Pruned {
		_, _ = fmt.Fprintf(w, "  %s\t%s\n", e.URI, dash(e.CapturedAt))
	}
	_ = w.Flush()
	for _, d := range res.RemovedDirs {
		_, _ = fmt.Fprintf(out, "removed dir %s\n", d)
	}
	_, _ = fmt.Fprintf(out, "skipped: %d unreviewed, %d undated\n", res.SkippedUnreviewed, res.SkippedUndated)
}

// promoteHint names the next step after a refused promotion, or "" for an
// error that is not one of PromoteJournal's refusals.
func promoteHint(err error) string {
	switch {
	case errors.Is(err, memory.ErrAlreadyPromoted):
		return " (already reviewed: leave the entry as is)"
	case errors.Is(err, memory.ErrInvalidPromoteTarget):
		return " (write the kb document first, or pass " + memory.PromoteNone + ")"
	case errors.Is(err, memory.ErrNotJournalEntry):
		return " (list reviewable entries with mnemos journal pending)"
	default:
		return ""
	}
}
