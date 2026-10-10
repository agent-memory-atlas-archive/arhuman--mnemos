# Consolidation — full procedure

Read this file before running a CONSOLIDATE pass. The goal is entropy
reduction: fewer, better documents — not summarizing for the sake of it.
A pass must never lose information silently: every merge keeps a source
trail, every removal is journaled, contradictions are surfaced, not settled.

## 0. Scope

- `mnemos.list(path: <capture_dir>)` — the inbox of raw captures. Add
  `unindexed_only` to catch files the watcher missed.
- `mnemos.journal_pending` lists session journal entries not yet reviewed,
  oldest first (see §7). Use it rather than `mnemos.list(path: journal)`:
  pending is a property of the entry, not of its path.
- If the inbox is empty and no journal entry is pending, report
  `Already present: inbox clean` and stop.
- Announce the plan first: "N captures, M journal entries to process". Process
  oldest first.

## 1. Triage — five buckets

For each capture: `mnemos.read` it, extract the key entities (project,
component, decision, task, constraint), then `mnemos.search` those entities
across the tree (excluding the capture dir). Route to exactly one bucket:

| Bucket | Test | Action |
|---|---|---|
| Already known | an existing doc fully covers it | prune the capture (§4) |
| Enrichment / new knowledge | adds to or creates a topic | merge or promote (§2) |
| Task | it is really an action item | create/update a task doc (see task-schema.md) |
| Decision | a choice with rationale | create/update `decisions/<slug>.md` |
| Transient noise | no durable value | prune, with one-line justification in the journal |

## 2. Merge and promote

- **One canonical document per topic.** Prefer enriching an existing
  decision/topic doc over creating a new one; create only when nothing covers
  the topic.
- Merging: `mnemos.read` the canonical doc, integrate the capture's content
  in place (not appended as a raw blob), then `mnemos.remember` the full
  updated doc. **Preserve the source trail**: note the original capture path
  and date in the doc (e.g. a `Sources` line).
- Promoting a wholly new note: `mnemos.move(capture, canonical_path)` keeps
  the file and its identity; then read-modify-write to clean up frontmatter
  (`type`, `title`, `tags`).
- **Link the network:** after writing, `mnemos.search` for adjacent docs and
  add a short `Related` section (markdown links) both ways when it helps
  future retrieval. Isolated notes don't get recalled.

## 3. Duplicates and contradictions

- **Duplicates:** `mnemos.search(topic, limit: 8)`. If several hits state the
  same fact, fold them into the canonical doc's single statement, list the
  folded URIs under `Sources`, and prune the redundant copies.
- **Contradictions — never resolve by overwrite.** The agent is not the
  arbiter of truth:
  1. in the canonical doc, add a `## Conflicts` section: claim A (citation),
     claim B (citation), `Resolution: unresolved` plus what evidence would
     settle it;
  2. open a review task (`status: todo`) referencing both URIs;
  3. keep the conflicting capture (move it under the capture dir's
     `archive/`, don't forget it).
  When the user later rules, apply the resolution via read-modify-write and
  mark the losing claim superseded — visibly, with the date.

## 4. Pruning

- Requires `allow_delete = true`. Prefer `mnemos.move` into
  `<capture_dir>/archive/` over `mnemos.forget`; reserve `forget` for pure
  noise. Watch the `dangling_links` count `move` returns — rewrite inbound
  links it reports.
- Without `allow_delete`: leave the capture in place and read-modify-write
  its frontmatter to `status: pending-prune`, so the next authorized pass
  can finish the job.
- Never prune a capture before its canonical version exists and is indexed.

## 5. Health check (optional, on request or when the pass is small)

- Orphans: docs with no inbound links (`mnemos.list` + link edges) —
  candidates for linking or archiving.
- Stale state: `in_progress` tasks untouched for weeks, `status.md` older
  than recent decisions. Open review tasks; don't auto-fix.

## 6. Close the pass

1. **Journal**: one dated entry, written once, never edited:
   `journal/<YYYY-MM-DD>-consolidation.md` (`type: document`,
   `tags: [journal, consolidation]`) listing captures processed, journal
   entries reviewed with their `promoted_to`, docs created/updated, prunes with
   reasons, conflicts opened.
2. **Report** to the user: `Created / Updated / Already present / Warnings`
   (omit empty sections; `Warnings` carries conflicts and dangling links).
3. **Commit**: in a jj repo, `jj new -m "chore: consolidate memory <date>"`
   — the VCS diff is the audit trail of the whole pass.

## 7. Journal entries

The session journal (entries of `type: journal` under `journal/<date>/`,
written by the Stop hook) is a second source for a pass. Entries are excluded
from search and become citable only through promotion (ADR-0011). Promotion and
stamping both need `allow_write = true`; without it, report the pending count
and the candidates, and write nothing.

1. **Read.** `mnemos.journal_pending`, then `mnemos.read(uri)` per entry. An
   entry holds `Request` (the user's words), `Files touched`, and `Outcome` (the
   assistant's final text, verbatim).
2. **Weigh the source.** `Request` is what the user said. `Outcome` is an
   agent's claim, not a confirmed fact. Promote only what the entry states
   explicitly: a decision the user made or confirmed, a constraint the user
   stated, a durable fact the touched files back up. Never infer a decision the
   entry does not state, and never promote the assistant's speculation. When
   unsure, do not promote.
3. **Triage** with the five buckets of §1. Most entries are transient noise.
4. **Promote** by writing or enriching the canonical document with
   `mnemos.remember` (§2), adding the entry uri to its `Sources` line.
   Contradictions follow §3: a `Conflicts` section and a review task, never an
   overwrite.
5. **Stamp**, only once the kb document is written and indexed:
   `mnemos.journal_promote(entry_uri, promoted_to: <kb document uri>)`. For noise
   or already-known content, `promoted_to: none`, so the entry is not re-read on
   the next pass. One stamp per entry: when an entry fed several documents, stamp
   the main one and cite the entry in every document's `Sources`. A stamped entry
   is refused by the tool; never try to re-stamp it.
6. **Leave entries in place.** Never move, forget or edit a journal entry during
   a pass. Pruning the journal is a separate, explicit command.

Add a `Journal: N reviewed, M promoted` line to the pass report.
