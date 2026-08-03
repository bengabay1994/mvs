# mvs — migrate agent session history between project directories

You named a project `projname1`, spent a week talking to Claude Code, Codex, Gemini CLI, and OpenCode inside it, then renamed the folder to `projname2`. Now `claude --resume`, `codex resume`, etc. show no history in the new folder — your transcripts are still on disk, just keyed by the old path.

`mvs` shows you every session every supported agent ever stored, lets you fuzzy-filter to the project you're looking for, asks where it should live now, and rewrites every file each agent needs touched so resume works from the new location.

Supported agents:

| Agent | What gets rewritten |
| --- | --- |
| Claude Code (`~/.claude`) | matching files under `projects/<encoded-cwd>/`, `cwd` field on every selected JSONL line, `history.jsonl`, `~/.claude.json` `.projects` key |
| Codex CLI (`~/.codex`)    | `payload.cwd` on `session_meta` and `turn_context` lines, `threads.cwd` in `state_5.sqlite` |
| Gemini CLI (`~/.gemini`)  | `tmp/<id>/`, `history/<id>/`, `.project_root`, `projectHash` in chat headers, `projects.json` |
| OpenCode (`~/.local/share/opencode`) | `session.directory`, `workspace.directory`, `project.worktree`/`sandboxes`, `snapshot/<id>/<sha1>/` rename |

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

If the installation directory is not on `PATH`, the script prints PowerShell commands that add it permanently to the current user's `PATH`. It does not silently modify the user's environment.

### Prebuilt binaries

`make release` produces single-file binaries under `dist/` for:

- linux/amd64
- linux/arm64
- darwin/amd64
- darwin/arm64
- windows/amd64

Drop one into `~/.local/bin` (Unix), or `$HOME\.local\bin` on Windows, and ensure that directory is on `PATH`. No runtime dependencies; SQLite is statically linked via `modernc.org/sqlite`.

## Usage

### Interactive TUI

```
mvs
```

Keys: type to fuzzy-filter, ↑/↓ to navigate, `space` to (de)select a project, `ctrl+a`/`ctrl+x` to toggle all, `enter` to continue, then type the new path, confirm.

### Non-interactive

```
mvs migrate /old/path /new/path                  # move (default)
mvs migrate --copy /old/path /new/path           # duplicate, leave originals (except OpenCode)
mvs migrate --agent claude /old/path /new/path   # scope to one agent
mvs migrate --dry-run /old/path /new/path        # show the plan, write nothing
```

### Other subcommands

```
mvs doctor       # show which agents were detected and where
mvs list         # print every (agent, cwd) group on disk
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

- `move` (default): the old cwd no longer shows the session; the new one does. Matches the "I renamed my project" workflow.
- `--copy`: both cwds show the session for Claude, Codex, and Gemini.
- OpenCode copy mode is deliberately disabled. Current OpenCode schemas contain additional session-owned tables, and cloning only the known tables can produce an incomplete session. Move mode is supported.

`mvs` reads the canonical cwd from session metadata rather than reverse-engineering encoded directory names. For Claude, only matching transcript files are moved out of a lossy encoded directory, so unrelated sessions that collide under the encoding remain in place.

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

Adding a new agent: implement the `adapter.Adapter` interface, register it in `adapter.All()`. The TUI and all subcommands pick it up automatically.

## Known limitations

- The TUI groups sessions by (agent, cwd). All sessions belonging to the same group migrate together; selecting only some of them isn't supported.
- Select at most one source path per agent in a run. This keeps backup and rollback boundaries unambiguous; run `mvs` again for another source path.
- Gemini refuses to merge two existing project stores. Migrate before opening Gemini in the new folder, or move one store separately after backing it up.
- OpenCode `--copy` is not supported; use move mode.
- `mvs` cannot reliably detect every running agent process across supported operating systems. Quit the running CLI before migrating or undoing.
- Body text inside transcripts (tool output, model responses) that mentions the old path is left untouched; only structural fields are rewritten. This is intentional — model-visible content is historical and rewriting it would corrupt the conversation record.
