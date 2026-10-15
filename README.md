# mvs — migrate agent session history between project directories

You want to continue a conversation in another workspace, or you renamed a project folder and its history disappeared from `claude --resume`, `codex resume`, etc. The transcripts are still on disk, keyed by their original project path.

`mvs` means **move session**. It lists individual conversations by default: pick one session (or several), choose a new project/workspace path, and resume there. Press `tab` in the list to switch to **project mode** when you want to migrate every session for an agent at a source path.

Supported agents:

| Agent | Session mode (default) | Project mode |
| --- | --- | --- |
| Claude Code (`~/.claude`) | Selected transcripts and companion/subagent files; prompt history identified by session ID | All matching sessions, prompt history, and the `~/.claude.json` project key |
| Codex CLI (`~/.codex`) | Selected rollout metadata and corresponding `threads` rows in `state_5.sqlite` | Every session at the source path |
| Gemini CLI (`~/.gemini`) | Selected chats, target ownership markers, and registry entry; source store retained | Entire `tmp/` and `history/` project stores, chat headers, and registry mapping |
| OpenCode (`~/.local/share/opencode`) | Selected session rows rebound to the target project/workspace; shared snapshots copied | Session/workspace/project paths and per-cwd snapshot directories |

## Install

### From source (Go 1.26, pinned by `mise.toml`)

macOS/Linux:

```
git clone https://github.com/bengabay1994/mvs
cd mvs
./install.sh        # builds and copies to ~/.local/bin/mvs
```

If `~/.local/bin` is not on your `PATH`, add this to your shell profile:

```
export PATH="$HOME/.local/bin:$PATH"
```

Windows PowerShell:

```powershell
git clone https://github.com/bengabay1994/mvs
Set-Location mvs
mise install
.\install.ps1       # builds and copies to $HOME\.local\bin\mvs.exe
```

If the installation directory is not on `PATH`, the script prints a command that adds it permanently to the current user's `PATH`. Pass `-AddToPath` to perform that opt-in update during installation. The script preserves expandable entries already present in the user's `PATH`.

### Prebuilt binaries

`make release` produces single-file binaries under `dist/` for:

- linux/amd64
- linux/arm64
- darwin/amd64
- darwin/arm64
- windows/amd64

Drop one into `~/.local/bin` on Unix. On Windows, rename the downloaded artifact to `$HOME\.local\bin\mvs.exe`. Ensure the applicable directory is on `PATH`. SQLite is statically linked via `modernc.org/sqlite`. OpenCode session transfers to unregistered targets use Git to resolve the destination project identity.

## Usage

### Interactive TUI

```
mvs
```

The default view lists individual sessions with a title (or ID), source path, and last activity. Type to fuzzy-filter by title, ID, path, or agent. Use ↑/↓ to navigate, `space` to select a session, and `enter` to enter the new path and review the plan.

Press `tab` on the list screen to toggle **sessions ↔ projects**. Project rows group all sessions by agent and source path. Switching views clears the selection. `ctrl+a` selects all filtered rows; `ctrl+x` clears the selection. On the target screen, `tab` completes paths. The confirmation screen lists the selected session IDs in session mode.

Selection scope (sessions/projects) is separate from the operation (move/copy). Move remains the default operation; use `mvs --copy` or press `c` on the confirmation screen for copy mode.

### Non-interactive

```
mvs list                                        # find session IDs
mvs migrate --agent claude --session ID /old/path /new/path
mvs migrate --session ID1 --session ID2 /old/path /new/path
mvs migrate --copy --session ID /old/path /new/path
mvs migrate --dry-run --session ID /old/path /new/path
mvs migrate --project /old/path /new/path         # all agents' sessions at the source
mvs migrate --project --agent claude /old/path /new/path
```

`migrate` requires `--session ID` (repeatable) or `--project`; these flags cannot be combined. A bare `mvs migrate FROM TO` fails with guidance instead of implicitly moving an entire project. Unknown or ambiguous session IDs also fail before any writes; use `--agent` to disambiguate IDs shared by agents.

### Other subcommands

```
mvs doctor       # show which agents were detected and where
mvs list         # print individual session IDs, paths, and titles
mvs list --project # print every (agent, cwd) group on disk
mvs undo RUN_ID  # restore from a backup snapshot
mvs undo         # list known backup runs
```

## Safety

Before any write, `mvs` snapshots every file and a consistent image of every SQLite database it intends to touch under a private directory:

```
~/.local/state/mvs/backups/<RUN_ID>/
```

and writes an `undo.json` manifest. To roll back:

```
mvs undo 20260525-122849
```

Each run uses a collision-resistant ID. If any adapter fails, `mvs` immediately restores all adapters already changed in that run. A manual `undo` fingerprints every changed file and database first; if an agent wrote new data after migration, undo refuses to overwrite it.

Quit all supported agents before migrating or undoing. `mvs` takes consistent SQLite snapshots, but it cannot stop another process from writing transcript files while they are being moved.

Move semantics:

- `move` (default): selected sessions appear at the new cwd. Unselected conversations remain at their original location.
- `--copy`: both cwds show the session for Claude, Codex, and Gemini.
- OpenCode copy mode is deliberately disabled. Current OpenCode schemas contain additional session-owned tables, and cloning only the known tables can produce an incomplete session. Move mode is supported.

`mvs` reads the canonical cwd from session metadata rather than reverse-engineering encoded directory names. For Claude, only matching transcript files are moved out of a lossy encoded directory, so unrelated sessions that collide under the encoding remain in place.

Session mode leaves shared source-project settings in place. In particular, Claude trust/tool grants stay with their project, and only history entries carrying a selected `sessionId` move. Gemini session transfers can add chats to an existing target store; project mode retains the stricter whole-store collision checks. OpenCode session transfers retain shared source workspace/project metadata and snapshots; a child moved without its parent becomes a root session at the destination.

## Why this isn't a one-liner

Each agent stores sessions differently. Renaming `~/.claude/projects/<encoded-old>/` to `<encoded-new>/` (as a few blog posts suggest) gets you part of the way, but you still lose:

- Trust-dialog acceptance and allowed-tool grants stored under `~/.claude.json .projects[<old cwd>]`
- Up-arrow prompt history (`~/.claude/history.jsonl`)
- The `payload.cwd` in every `turn_context` line of a Codex rollout, plus the `threads.cwd` column in `state_5.sqlite` that newer Codex builds use as the primary index
- The `projectHash` and `.project_root` markers in Gemini's per-project dir
- OpenCode's `session.directory`, workspace/project path metadata, and per-cwd snapshot key. Historical message and part bodies are intentionally not rewritten.

`mvs` handles those as one recoverable run. File replacements and database updates are atomic, and a failed run is rolled back automatically.

## Development

```
make build                     # ./mvs
make release                   # cross-compile all platforms to dist/
make tidy                      # go mod tidy
mise exec -- go test ./...
mise exec -- go test -race ./...
```

Project layout:

```
main.go                  CLI dispatcher
internal/session/        cross-agent Session / Plan / Report types
internal/paths/          per-OS path resolution
internal/adapter/        one file per agent (claude.go, codex.go, gemini.go, opencode.go)
internal/backup/         per-run snapshot + undo manifest
internal/migration/      preflight + automatic rollback orchestration
internal/fsutil/         atomic replacement + drift fingerprints
internal/tui/            Bubble Tea model (scan → list → target → confirm → apply)
```

Adding a new agent: implement the `adapter.Adapter` interface for both session and project scopes, register it in `adapter.All()`, and add its restore policy in `internal/backup/backup.go`.

## Known limitations

- Select at most one source path per agent in a run. This keeps backup and rollback boundaries unambiguous; run `mvs` again for another source path.
- Gemini project mode refuses to merge two existing project stores. Session mode accepts existing targets but refuses conversation filename collisions. Stores with no discoverable conversations are available only in project mode.
- OpenCode session moves copy shared snapshots when present, reuse an identical target snapshot, and refuse conflicting snapshot stores. Resolving an unregistered target's identity requires Git on `PATH`; existing registered target projects need no Git lookup.
- OpenCode `--copy` is not supported; use move mode.
- `mvs` cannot reliably detect every running agent process across supported operating systems. Quit the running CLI before migrating or undoing.
- Body text inside transcripts (tool output, model responses) that mentions the old path is left untouched; only structural fields are rewritten. This is intentional — model-visible content is historical and rewriting it would corrupt the conversation record.
