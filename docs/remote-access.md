# Remote and centralized access

mnemos never opens a network listener. `mnemos serve` speaks MCP over stdio only.
To use one workspace from several machines, run mnemos on the machine that hosts the
docs and have each client launch it over SSH. SSH provides authentication and
encryption; mnemos adds nothing on top.

Use this when the docs live in one place (a shared checkout, a documentation
server) and you want Claude Code on other machines to search and cite them. For a
single machine, the plain setup in the [README](../README.md#connect-claude-code) is
enough.

## Setup on the host

1. Create one dedicated Unix account, for example `mnemos`, that owns the workspace.
   Every `mnemos serve` process must run as this account: it owns the database files
   under `state/` and the `kb/` tree.
2. Install the binary on that account's `PATH` and initialize the workspace:

   ```bash
   mnemos --mnemos-dir /srv/docs/.mnemos init
   mnemos --mnemos-dir /srv/docs/.mnemos origin add /srv/docs/repo --prefix docs --collection docs
   ```

3. Add each client's public key to `~mnemos/.ssh/authorized_keys`. Key-based auth is
   required; the client never gets a password prompt (see below).

Concurrent sessions are separate `mnemos serve` processes on the host. They share one
SQLite database in WAL mode with `BEGIN IMMEDIATE` write transactions, which
serializes writers safely ([ADR 0012](adr/0012-write-transaction-locking.md)).

## Connect Claude Code

Pin the workspace with an absolute `--config` on the remote side. Client environment
variables such as `$CLAUDE_PROJECT_DIR` and `$MNEMOS_DIR` are not forwarded by SSH, so
without it discovery falls back to the remote user's `~/.mnemos`.

```bash
claude mcp add mnemos -- ssh -T -o BatchMode=yes mnemos@docs-host \
  mnemos --config /srv/docs/.mnemos/mnemos.toml serve
```

The `.mcp.json` equivalent:

```json
{
  "mcpServers": {
    "mnemos": {
      "command": "ssh",
      "args": ["-T", "-o", "BatchMode=yes", "mnemos@docs-host",
               "mnemos", "--config", "/srv/docs/.mnemos/mnemos.toml", "serve"]
    }
  }
}
```

`-T` disables the pseudo-terminal. `BatchMode=yes` makes ssh fail instead of waiting
for a password or host-key prompt, which would hang the MCP spawn.

Cited URIs are relative to the kb root (or `<prefix>/...` for a registered origin),
not absolute host paths. A remote client cannot open them locally; it reads cited
content through the `mnemos.read` tool.

## Restrict what a key may run

A bare key gives a shell. To limit a key to mnemos, force a command in
`authorized_keys`:

```text
command="/usr/local/bin/mnemos-ssh",restrict ssh-ed25519 AAAA... alice@laptop
```

The hooks and CLI need more than `serve`, so the forced command is a small
dispatcher. It allowlists the subcommand, and the hook subcommands one by one (`hook
stop` reads a transcript path from its payload, so it stays local; a new hook
subcommand is denied until listed). It rejects any client-supplied `--config` or
`--mnemos-dir` (so the client cannot pick another workspace), and pins the workspace
itself:

```sh
#!/bin/sh
# /usr/local/bin/mnemos-ssh
set -eu
set -f
# Intentional word splitting of the client command; globbing is off.
# shellcheck disable=SC2086
set -- ${SSH_ORIGINAL_COMMAND:-}

[ "${1:-}" = "mnemos" ] && shift

case "${1:-} ${2:-}" in
  "hook session-start"|"hook recall") ;;
  serve\ *|search\ *|task\ *|ls\ *|related\ *|status\ *) ;;
  *) echo "mnemos-ssh: command not allowed" >&2; exit 1 ;;
esac

for arg in "$@"; do
  case "$arg" in
    --config*|--mnemos-dir*)
      echo "mnemos-ssh: workspace flags not allowed" >&2; exit 1 ;;
  esac
done

exec mnemos --config /srv/docs/.mnemos/mnemos.toml "$@"
```

With the dispatcher in place, clients send `mnemos serve` and drop `--config` from the
commands in [Connect Claude Code](#connect-claude-code): the dispatcher pins the
workspace and rejects a client-supplied `--config`. The hook and wrapper commands
below assume the dispatcher.

`mnemos search` joins its arguments with spaces, so SSH flattening a multi-word query
into separate words is harmless. The script must be executable and not writable by
the clients' accounts.

## Hooks and the skill

The hooks in
[`skills/mnemos-okf/hooks/settings.example.json`](../skills/mnemos-okf/hooks/settings.example.json)
read hook JSON on stdin and write markdown on stdout; ssh forwards both. For a
remote workspace, replace the local call with an ssh call (the dispatcher pins
`--config`):

```text
ssh -T -o BatchMode=yes mnemos@docs-host mnemos hook session-start 2>/dev/null; exit 0
ssh -T -o BatchMode=yes mnemos@docs-host mnemos hook recall 2>/dev/null; exit 0
```

To let the skill's CLI calls (`mnemos task list`, `mnemos ls decisions`,
`mnemos search`) target the remote workspace, put a wrapper named `mnemos-remote` on
the client's `PATH`:

```sh
#!/bin/sh
exec ssh -T -o BatchMode=yes "${MNEMOS_SSH_HOST:?}" mnemos "$@"
```

Set `MNEMOS_SSH_HOST=mnemos@docs-host` and call `mnemos-remote` where the skill would
call `mnemos`.

Each hook invocation opens an SSH connection. Reuse one with OpenSSH multiplexing in
`~/.ssh/config`:

```text
Host docs-host
  ControlMaster auto
  ControlPath ~/.ssh/cm-%r@%h:%p
  ControlPersist 10m
```

## Writes and permissions

`[mcp] allow_write` and `allow_delete` live in the workspace's `mnemos.toml` and
apply to every session of that workspace. There is no per-key or per-user override,
and no environment variable or flag changes them. Mixed privileges on one workspace
(some clients read-only, some read-write) are not supported.

For a shared doc server, keep both gates `false` (the default) and update content on
the host: `git pull` followed by `mnemos origin reindex <prefix>`, `mnemos watch`, or
`mnemos add`.

## Operational notes

- **stdout is the transport.** Anything the remote side prints to stdout before or
  during the session (shell rc files, MOTD, banners, `echo` in a profile) corrupts
  the MCP stream. A forced command still runs through the account's login shell, so
  its startup files run too: keep them silent, or give the account a minimal shell
  such as `/bin/sh`.
- **Startup cost.** Each session starts a fresh process. With
  `[search] use_vectors = true` (embed build) the embedding model loads per session,
  which adds startup latency. This is the main downside of the model.
- **One account.** All processes must share the account that owns the workspace.
  Several accounts writing to the docs tree break the single-owner assumption; see
  [SECURITY.md](../SECURITY.md#known-limitations).

## Why not HTTP/SSE

mnemos provides no network transport. SSE is deprecated in the MCP specification
(2025-03-26) in favor of Streamable HTTP, and an HTTP listener would need its own
authentication, TLS, and origin handling, which SSH already supplies. Adding a
network transport would be a separate decision recorded in an ADR.
