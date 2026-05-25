# mvs — migrate agent session history between project directories

You named a project `projname1`, spent a week talking to Claude Code, Codex, Gemini CLI, and OpenCode inside it, then renamed the folder to `projname2`. Now `claude --resume`, `codex resume`, etc. show no history in the new folder — your transcripts are still on disk, just keyed by the old path.

`mvs` shows you every session every supported agent ever stored, lets you fuzzy-filter to the project you're looking for, asks where it should live now, and rewrites every file each agent needs touched so resume works from the new location.

Supported agents:

| Agent | What gets rewritten |
| --- | --- |
| Claude Code (`~/.claude`) | `projects/<encoded-cwd>/` rename, `cwd` field on every JSONL line, `history.jsonl`, `~/.claude.json` `.projects` key |
| Codex CLI (`~/.codex`)    | `payload.cwd` on `session_meta` and `turn_context` lines, `threads.cwd` in `state_5.sqlite` |
| Gemini CLI (`~/.gemini`)  | `tmp/<id>/`, `history/<id>/`, `.project_root`, `projectHash` in chat headers, `projects.json` |
| OpenCode (`~/.local/share/opencode`) | `session.directory`/`path`, `project.worktree`, `part.data`/`message.data` REPLACE, `snapshot/<id>/<sha1>/` rename |

## Install

### From source (any platform with Go ≥ 1.22)

```
git clone https://github.com/bengabay1994/mvs
cd mvs
./install.sh        # builds and copies to ~/.local/bin/mvs
```

If `~/.local/bin` is not on your `PATH`, add this to your shell profile:

```
export PATH="$HOME/.local/bin:$PATH"
```

### Prebuilt binaries

`make release` produces single-file binaries under `dist/` for:

- linux/amd64
- linux/arm64
- darwin/amd64
- darwin/arm64
- windows/amd64

Drop one into `~/.local/bin` (Unix) or anywhere on `%PATH%` (Windows) and you're done. No runtime dependencies; SQLite is statically linked via `modernc.org/sqlite`.

## Usage

### Interactive TUI

```
mvs
```

Keys: type to fuzzy-filter, ↑/↓ to navigate, `space` to (de)select a project, `ctrl+a`/`ctrl+x` to toggle all, `enter` to continue, then type the new path, confirm.

### Non-interactive

```
mvs migrate /old/path /new/path                  # move (default)
mvs migrate --copy /old/path /new/path           # duplicate, leave originals
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

Before any write, `mvs` snapshots every file/DB it intends to touch under:

```
~/.local/state/mvs/backups/<RUN_ID>/
```

and writes an `undo.json` manifest. To roll back:

```
mvs undo 20260525-122849
```

Move semantics:

- `move` (default): the old cwd no longer shows the session; the new one does. Matches the "I renamed my project" workflow.
- `--copy`: both cwds show the session. Implemented per-agent — for Codex/OpenCode this duplicates database rows with fresh IDs, for Claude/Gemini it duplicates the relevant entries in `~/.claude.json` / `projects.json`.

`mvs` reads the canonical cwd from inside each session file rather than reverse-engineering encoded directory names, so collisions caused by lossy encodings (e.g. Claude's "every non-alphanumeric → `-`") are handled correctly.

## Why this isn't a one-liner

Each agent stores sessions differently. Renaming `~/.claude/projects/<encoded-old>/` to `<encoded-new>/` (as a few blog posts suggest) gets you part of the way, but you still lose:

- Trust-dialog acceptance and allowed-tool grants stored under `~/.claude.json .projects[<old cwd>]`
- Up-arrow prompt history (`~/.claude/history.jsonl`)
- The `payload.cwd` in every `turn_context` line of a Codex rollout, plus the `threads.cwd` column in `state_5.sqlite` that newer Codex builds use as the primary index
- The `projectHash` and `.project_root` markers in Gemini's per-project dir
- The `session.directory`/`session.path` columns and every embedded path inside `part.data`/`message.data` in OpenCode's SQLite

`mvs` handles all of those in one shot, atomically, with a backup you can undo.

## Development

```
make build       # ./mvs
make release     # cross-compile all platforms to dist/
make tidy        # go mod tidy
go test ./...    # (no tests yet)
```

Project layout:

```
main.go                  CLI dispatcher
internal/session/        cross-agent Session / Plan / Report types
internal/paths/          per-OS path resolution
internal/adapter/        one file per agent (claude.go, codex.go, gemini.go, opencode.go)
internal/backup/         per-run snapshot + undo manifest
internal/tui/            Bubble Tea model (scan → list → target → confirm → apply)
```

Adding a new agent: implement the `adapter.Adapter` interface, register it in `adapter.All()`. The TUI and all subcommands pick it up automatically.

## Known limitations

- The TUI groups sessions by (agent, cwd). All sessions belonging to the same group migrate together; selecting only some of them isn't supported.
- Claude's encoded directory name is lossy. If two distinct cwds previously encoded to the same dirname, `mvs` cannot tell them apart by directory name alone — it reads the canonical cwd from inside each JSONL.
- `mvs` refuses no operations if an agent is currently running. Quit the running CLI before migrating; otherwise SQLite WAL files may be re-checkpointed over your edits.
- Body text inside transcripts (tool output, model responses) that mentions the old path is left untouched; only structural fields are rewritten. This is intentional — model-visible content is historical and rewriting it would corrupt the conversation record.
