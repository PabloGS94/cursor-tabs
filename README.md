# cursor-tabs

Minimal terminal session manager for Cursor Agent with left/right tab navigation.

## What it does

- Discovers git repos and shows them as tabs on top.
- Runs any number of persistent `tmux` sessions per repo, listed under the tab.
- Starts Cursor agent (`agent` or `cursor-agent`) inside each session.
- Lets you attach/detach quickly.

Each tab shows a colored dot summarizing its sessions: red = error,
yellow = needs input, green = working, dim = idle. No dot = no sessions.

## Requirements

- `tmux`
- Cursor CLI command (`agent` or `cursor-agent`)
- Go 1.23+

## Run

```bash
cd ~/dev/cursor-tabs
go run .
```

## Keys

- `left` / `right` (or `h` / `l`): switch project tab
- `up` / `down` (or `j` / `k`): move between sessions in the current project
- `enter`: open selected session (starts one if the project has none)
- `n`: start an additional session for the current project and open it
- `ctrl+q`: detach from a session, back to the list (single keypress)
- `ctrl+x` twice: stop selected session (first press shows a red confirm hint)
- `r`: refresh
- `q`: quit

## Statuses

- `Working` (green): agent is actively doing something
- `Needs input` (yellow): agent is waiting for you
- `Idle` (green): session open, nothing running
- `Error` (red): something went wrong

## Configuration

By default, repos are discovered under `~/dev`.

Optional environment variables:

- `CURSOR_TABS_ROOT` - root dir to scan for git repos
- `CURSOR_TABS_REPOS` - explicit comma-separated repo paths

Examples:

```bash
CURSOR_TABS_ROOT=~/work go run .
```

```bash
CURSOR_TABS_REPOS=~/dev/homepage,~/dev/giftcards go run .
```

## Notes

- Status inference is heuristic (`running`, `waiting`, `idle`, `error`).
- Sessions are persistent because they live in `tmux`, not in the TUI process.
