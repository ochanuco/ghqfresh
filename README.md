# ghqfresh

Keeps the default branch of every [ghq](https://github.com/x-motemen/ghq)-cloned
GitHub repository of one owner up to date. It only ever fast-forwards, and it
leaves alone any clone that is in use.

```sh
go install github.com/ochanuco/ghqfresh@latest
ghqfresh                 # refresh every clone of $GHQFRESH_OWNER (default: ochanuco)
ghqfresh -changed        # refresh only repositories pushed since their last sync
```

## What it does to a clone

For each `github.com/<owner>/<name>` clone, ghqfresh fetches the repository's
default branch (as GitHub reports it, so `main` and `master` both work) into
`origin/<branch>` and then moves the local branch forward:

- If the branch is not checked out anywhere, only the ref moves.
- If it is checked out — in the clone itself or in any of its worktrees — that
  worktree is fast-forwarded, unless it has local changes to tracked files or a
  git operation in progress (`index.lock`, rebase, merge, cherry-pick, revert,
  bisect). Those are reported as `skip` and retried next run.
- If the local branch has commits origin does not, it is reported as diverged
  and left as is.

Authentication uses the `gh` token over HTTPS, passed through `GIT_CONFIG_*`
environment variables, so no SSH agent prompt appears and the token is written
neither to argv nor to `.git/config`. Every git command has a time limit
(`-timeout`).

## Scheduled runs

`-changed` is meant for a scheduler such as launchd. One GraphQL query lists
when each of the owner's repositories was last pushed; only clones pushed since
their last successful sync are fetched, so idle repositories cost nothing. Once
the last full run is older than `-full-every` (24h), a `-changed` run refreshes
everything again.

When GitHub is unreachable — typically right after the machine wakes — a
`-changed` run exits 0 without output and the next run picks up. A run that
finds another run in progress also exits quietly.

Output is coloured with a progress line on a terminal. Otherwise every result
except `ok` is written as a timestamped line, so an idle scheduled run writes
nothing.

| Flag | Default | |
|---|---|---|
| `-owner` | `$GHQFRESH_OWNER`, else `ochanuco` | GitHub owner whose clones are refreshed |
| `-changed` | off | refresh only repositories pushed since their last sync |
| `-full-every` | `24h` | with `-changed`, how often everything is refreshed anyway |
| `-jobs` | `8` | clones refreshed in parallel |
| `-timeout` | `2m` | time limit for each git command |
| `-state` | `$XDG_STATE_HOME/ghqfresh/state.json` | where sync times are kept |

Requires `git`, `ghq` and an authenticated `gh` on `PATH`.
