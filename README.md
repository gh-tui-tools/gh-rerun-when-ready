# gh-rerun-when-ready

A `gh` extension that re-runs a pull request’s failed CI jobs as soon as GitHub will allow it.

**Use-case:** Because GitHub refuses to re-run the failed jobs of a workflow run while any of that run’s other jobs are still going, re-running means watching the PR until that run has finished — and only then pressing the <kbd><b>Re-run failed jobs</b></kbd> button. This tool waits for that moment, and then presses that button.

## Install

```sh
gh extension install gh-tui-tools/gh-rerun-when-ready
```

Or, from a local clone:

```sh
go build -o gh-rerun-when-ready . && gh extension install .
```

## Use

```sh
gh rerun-when-ready https://github.com/OWNER/REPO/pull/123
gh rerun-when-ready 123 --repo OWNER/REPO
gh rerun-when-ready 123                  # from inside a clone of the repo
```

It polls the PR and watches each workflow run separately, re-running the failed jobs of any run that failed, timed out, or was cancelled — as soon as that particular run finishes. It doesn’t wait on other runs.

| Option | Meaning |
| --- | --- |
| `-R` `--repo OWNER/REPO` | Repository, when the PR is given as a number, and the working directory isn’t a clone of it |
| `-i` `‑‑interval SECONDS` | Seconds between checks (default: 60) |
| `-a` `--attempts N` | How many times to re-run a run that keeps failing (default: 1) |
| `-t` `--timeout SECONDS` | Give up after this long (default: no limit) |
| `-n` `--dry-run` | Report what would be re-run, re-run nothing, and exit without waiting |
| `-h` `--help` | Show the help message |

If new commits are pushed while it’s waiting, it notices the head has moved — and starts watching the new commit instead. A run that GitHub declines to re-run is reported rather than retried — so it can’t spin.

A request that gets no answer within 30 seconds (or whose connection drops), or that GitHub fails on its own side, doesn’t end the watch: It’s retried, with a growing wait between tries — and the watch gives up only once that request has kept failing for five minutes.

Exit status is 0 when every run for the head commit has succeeded, and 1 when runs are still failing after the allotted attempts — or when GitHub declined to re-run one. Those two endings are reported separately — so a spent attempt budget never reads as a refusal.

## Develop

```sh
go vet ./...
gofmt -l .
go test ./...
```
