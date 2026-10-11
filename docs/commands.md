# Command reference

Every `mnemos` CLI command. Note the spelling split: `mnemos.search` (dot) is the
**MCP tool** Claude calls; `mnemos search` (space) is the **CLI command** you run.

## Global flags

These apply to every command and select the workspace it acts on:

| Flag | Effect |
|---|---|
| `--mnemos-dir <dir>` | Explicit MNEMOS_DIR; overrides `$MNEMOS_DIR`, the project `./.mnemos`, and the `~/.mnemos` default |
| `--config <file>` | Explicit `mnemos.toml` path; its directory becomes the MNEMOS_DIR |
| `-v`, `--verbose` | Enable debug logging |

## Workspace

| Command | Purpose |
|---|---|
| `mnemos init [--global]` | Scaffold a workspace (`mnemos.toml`, `kb/`, `state/`, `models/`). Creates a project-local `./.mnemos` by default; `--global` targets `~/.mnemos` |
| `mnemos migrate --from <old-root-or-config> [--to <dir>] [--move]` | Move a pre-MNEMOS_DIR workspace into the `kb/` layout and reindex. Copies by default; `--move` relocates the source |
| `mnemos status` | Show the resolved MNEMOS_DIR and how it was chosen, plus collection/document/chunk counts and FTS availability |
| `mnemos version [-v]` | Print the version; `-v` adds commit, build date, and Go toolchain |

## Getting content in

| Command | Purpose |
|---|---|
| `mnemos add <source> [--into <subpath>] [--mode copy\|link] [--collection <c>]` | Bring **external** content into the kb and index it. `--mode copy` (default) snapshots it; `--mode link` symlinks a single file. `--into` picks the destination subpath (default: the source's base name) |
| `mnemos ingest <kb-subpath> [--collection <c>]` | Re-index content **already inside** the kb. A path outside the kb is refused, since it would mint URIs that `read`/`ls`/`mv` cannot resolve. A directory ingest also evicts documents under it whose file is gone; a single-file ingest does not |
| `mnemos watch <kb-subpath> [--collection <c>]` | Watch a path and incrementally reindex changed and removed files |
| `mnemos okfy <file> [--collection --type --tags --out --force]` | Convert an in-kb `.txt`/`.md` file into an OKF document, then index it (the source is kept intact) |

A document's `collection:` frontmatter is authoritative; `--collection` is only the
fallback for files that don't declare one.

## Query

| Command | Purpose |
|---|---|
| `mnemos search <query> [--collection --path --type --since --limit --semantic --json]` | Search the index and print cited, ranked results (`--semantic` fuses lexical + vector; needs the embed build) |
| `mnemos ls [path] [--collection --type --tree --depth --all --indexed --unindexed --limit --json]` | List and browse the OKF tree, annotated with index metadata |
| `mnemos related <uri> [--direction outbound\|inbound\|both] [--limit --json]` | List a document's 1-hop link-graph neighbors (outbound links and inbound backlinks) |
| `mnemos task list [--status <s>] [--collection <c>]` | List indexed Task documents grouped by status |

## Manage the tree

`forget` and `mv` delete index entries, so both require `[mcp] allow_delete = true`.
`edit` writes documents and requires `[mcp] allow_write = true`.

| Command | Purpose |
|---|---|
| `mnemos edit [uri]` | Edit one OKF document in an interactive terminal editor (nav / metadata / content panes). Requires a uri: bare `mnemos edit` errors |
| `mnemos forget <path>` | Delete a file from the OKF tree, on disk and in the index; idempotent |
| `mnemos mv <src> <dst>` | Move a file or directory within the OKF tree, renaming on disk and re-indexing under the new path |

## Session journal

Journal entries (written by `mnemos hook stop`) are excluded from search and reach
the kb only by promotion ([ADR-0011](adr/0011-journal-tier.md)). These commands
drive that review; `promote` writes and requires `[mcp] allow_write = true`, and
`prune --apply` deletes and requires `[mcp] allow_delete = true`.

| Command | Purpose |
|---|---|
| `mnemos journal pending [--limit N] [--json]` | List journal entries not yet reviewed (no `promoted_to`), oldest first |
| `mnemos journal promote <entry-uri> <target>` | Stamp `promoted_to` on a journal entry: the uri of the kb document now holding its content (write it first), or `none` when nothing was worth keeping. An entry already stamped is refused |
| `mnemos journal prune --older-than DURATION [--apply] [--json]` | Delete reviewed journal entries older than DURATION (a Go duration such as `12h`, or days such as `30d`). Age is measured from `captured_at`, or from the file's last modification when the entry has none; an entry with neither is kept and counted. Dry run by default: it lists what would go. `--apply` deletes and requires `[mcp] allow_delete = true`. Only entries already stamped with `promoted_to` are eligible; mark unreviewed ones with `mnemos journal promote` first. A dated directory left holding only its `log.md` is removed with it |

## Maintenance and diagnosis

| Command | Purpose |
|---|---|
| `mnemos doctor [path] [--collection --path --max-bytes --json --fail-on-findings]` | Report knowledge-base health issues, read-only. Includes `missing-file`: an indexed document whose backing file is gone (a deleted file, or a symlink whose target was removed). `--max-bytes` flags oversized documents (default 51200); `--fail-on-findings` exits non-zero for CI |
| `mnemos origin add <abs-dir> --prefix <ns> [--collection <c>]` | Register an external directory and index it **in place**, read-only, under the uri namespace `<ns>/`. Nothing is copied into the kb and nothing is ever written to the tree |
| `mnemos origin list [--json]` | List registered origins with their path, collection, last-indexed time, and whether the root is currently readable. `--json` exports a registration set that can be replayed |
| `mnemos origin reindex [prefix]` | Re-index registered origins in place (all, or one). Picks up new files and evicts deleted ones. A missing root fails and evicts nothing |
| `mnemos origin remove <prefix>` | Unregister an origin and evict its documents. The files on disk are never touched |
| `mnemos reindex [--content] [--embeddings]` | Recompute derived indexes. `--content` re-parses every indexed document from its file (bypassing the unchanged-file skip) and evicts documents whose file is gone; `--embeddings` recomputes and stores vectors for all chunks |
| `mnemos validate <bundle> [--json]` | Validate an OKF v0.1 bundle for conformance |
| `mnemos models install <model>` | Download an embedding model (e.g. `all-MiniLM-L6-v2`) into `<MNEMOS_DIR>/models` (for the embed build) |

## Serving and agent integration

| Command | Purpose |
|---|---|
| `mnemos serve` | Run the MCP server (stdio), exposing the search, read, context, related, list, journal_pending, remember, okfy, journal_promote, forget, and move tools |
| `mnemos hook session-start [--max-tokens N]` | Claude Code `SessionStart` hook: inject the scoped working set (default cap 1500 tokens) |
| `mnemos hook recall [--limit N] [--max-tokens N]` | Claude Code `UserPromptSubmit` hook: inject cited recall (defaults: 3 results, 600 tokens) |
| `mnemos hook stop` | Claude Code `Stop` hook: write one journal entry for the turn that just ended (request, files touched, final assistant text) under `journal/<date>/`. Requires `[mcp] allow_write = true` |

All hook subcommands stay silent (exit 0, no output) when the project has no
mnemos workspace or nothing relevant to do. `recall` and `stop` read the hook
JSON from stdin and ignore a payload over 1 MiB the same way.
`session-start` and `recall` inject scoped markdown on stdout. `stop` prints
nothing: it reads the transcript named in the payload and writes through the same
path as `mnemos.remember`, so the entry is secret-scanned and dropped (not
failed) when a secret is found or writes are disabled. Re-running it on the same
turn updates that turn's entry instead of adding one, except that an entry already
reviewed (a non-empty `promoted_to`) or whose frontmatter cannot be read is left
untouched. A turn starts at the last user prompt holding text or an attachment;
a tool result does not start one. A prompt with only attachments, such as a
pasted image, is recorded as its block types in brackets (`[image]`).
[ADR-0011](adr/0011-journal-tier.md) is the full contract.

## Evaluation

| Command | Purpose |
|---|---|
| `mnemos eval <bundle> [--baseline <f> --save --semantic --limit K]` | Held-out retrieval-quality eval on an OKF bundle (`--semantic` evaluates the hybrid retriever) |
| `mnemos eval <bundle> --graph [--limit K --seed-depth N]` | Graph-answerability eval: reads `<bundle>/cases.json` and measures link-neighborhood inclusion and actual GraphRetriever hit@K vs plain lexical search |
| `mnemos eval <bundle> --graph-expansion` | Held-out eval with link-neighbor expansion enabled (the Phase 2 non-regression gate: Hit@1 / Recall / MRR vs the plain baseline) |

See also [configuration.md](configuration.md) for `<MNEMOS_DIR>/mnemos.toml`, and
[paths-and-indexing.md](paths-and-indexing.md) for how state is located and how URIs
are resolved. For running commands against a remote workspace over SSH, see
[remote-access.md](remote-access.md).
