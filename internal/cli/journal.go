package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/arhuman/mnemos/internal/app"
	"github.com/arhuman/mnemos/internal/memory"
)

// newJournalCmd builds the `journal` command: `pending` lists the session
// journal entries not yet reviewed, and `promote` records where a reviewed
// entry's content went (ADR-0011). promote never writes the kb document; it
// stamps the entry after that document exists, and is gated by allow_write.
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
			"already written from it, or \"none\" when nothing was worth promoting. It does not write " +
			"the kb document. Requires [mcp].allow_write=true.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJournalPromote(cmd, state, args[0], args[1])
		},
	}

	cmd.AddCommand(pending, promote)

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
			return fmt.Errorf("journal promote: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s promoted_to %s\n", res.EntryURI, res.PromotedTo)

		return nil
	})
}
