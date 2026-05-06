# gh-extensions

Four `gh` CLI extensions for everyday GitHub workflow hygiene. Each one targets
a recurring source of silent failure or manual triage that costs minutes per
occurrence and adds up.

| Extension | What it does |
| --- | --- |
| `gh protection-audit` | Diffs a branch's required `status_checks.contexts` against the actual check-runs of a recent commit, with two heuristics for the most common drift causes (`#`-truncation in job names, matrix-display fallback). Catches the silent BLOCKED state that surfaces only as "Expected — Waiting for status to be reported". |
| `gh xref-verify` | Verifies the `(PR #N, Issue #M, merge <sha>)` triple is bidirectionally consistent before pasting a citation into permanent docs. PR's `closingIssuesReferences` ↔ Issue's `closedByPullRequestsReferences` plus optional merge-commit SHA. |
| `gh ci-triage` | Classifies a PR's CI failures as `PRE-EXISTING FLAKY` or `LIKELY REGRESSION` by sampling main's recent runs and (when possible) attributing the failing test file to a commit inside or outside the PR. Pluggable log adapter (currently pytest only). |
| `gh gist-content` | Prints one file's contents from a gist by ID, with a sanity check that catches the `gh gist view --raw` description-leak gotcha (description prepended to file content). Exit 1 on a sanity mismatch so it composes safely in pipelines. |

## Install

This repo is a Go monorepo with one binary per extension under `cmd/`. To
install one locally:

```sh
cd cmd/<extension>
go build -o <extension> .
gh extension install .
```

For example:

```sh
cd cmd/gh-gist-content && go build -o gh-gist-content . && gh extension install .
```

Repeat per extension as needed. Re-running `go build` after a code change is
all that's required — `gh extension install` creates a symlink.

Requirements:

- Go ≥ 1.25 (matches `go.mod`).
- A working `gh` CLI authenticated against the host you target.
- For `gh protection-audit` and `gh ci-triage`, run from inside a working
  clone OR pass `--repo OWNER/NAME` explicitly.

## Common exit-code regime

All four extensions share the same exit-code convention:

| Code | Meaning |
| --- | --- |
| `0` | Success or "no action needed" |
| `1` | Verification failure — semantic mismatch, sanity break, regression |
| `2` | Usage error — missing or invalid arguments |
| `3` | Upstream failure — API 404 / 5xx, network, missing git context |

This makes shell composition predictable, e.g.:

```sh
gh xref-verify 42 100 deadbee && publish-citation
gh ci-triage --workflow=lint.yml || investigate-regression
```

## Per-command quick reference

### `gh protection-audit`

```
gh protection-audit [--branch main] [--ref HEAD] [--repo OWNER/NAME]
```

Prints three sections: required contexts with no matching check-run, run-side
names that protection does not pin, and the timestamp of the audit. Each
missing required context can pick up a `← suspect:` annotation pointing at the
likely cause. Exits 1 when the missing-required section is non-empty.

### `gh xref-verify`

```
gh xref-verify <PR> <Issue> [<expected-sha>] [--repo OWNER/NAME]
```

Runs four checks (PR closes Issue, Issue closed by PR, merge SHA matches if
provided, both states are MERGED/CLOSED). On success emits a citation block
ready to paste into a doc; on failure lists the failing checks on stderr with
a `✗` prefix and exits 1.

### `gh ci-triage`

```
gh ci-triage [<PR>] [--workflow <name>] [--samples 8] [--main-branch main] [--repo OWNER/NAME]
```

Without `<PR>` it resolves the PR for the current branch via `gh pr view`. For
each failed workflow it samples main's last N runs (default 8), pulls the
failing run's log, and applies the verdict rule:

- main 0 fails / N runs → `LIKELY REGRESSION (caused by this PR)`
- main ≥ 1/5 fails AND failing test file last-touched outside the PR
  (or unknown) → `PRE-EXISTING FLAKY`
- otherwise → `GREY ZONE` with a reason

Any `LIKELY REGRESSION` verdict escalates the exit to 1 — so the binary
composes in scripts that should refuse to deploy on a regression.

For repos using `trunk` (or any non-`main` integration branch), pass
`--main-branch trunk`.

### `gh gist-content`

```
gh gist-content <id> <filename> [--no-sanity]
```

Prints the file's content. With sanity on (default), checks the first
non-whitespace token against an extension-driven prefix table:

- `.html` / `.htm` → `<!`
- `.json` → `[` or `{`
- `.xml` / `.svg` → `<?` or `<`
- `.py` → shebang or `import` / `from` / `def` / `class`

A mismatch emits a warning to stderr and exits 1. `--no-sanity` skips the
check (still exits 0 even if the content "looks weird").

## Repository layout

```
cmd/<extension>/         one main package per binary
internal/exitcode        shared 0/1/2/3 regime
internal/ghclient        thin go-gh wrapper (REST client + gh.Exec runner)
internal/logparse        log-extraction adapter table (gh-ci-triage)
testdata/<phase>/        JSON / log fixtures, indexed by extension code
```

## Development

Run the test suite from the repo root:

```sh
go test ./...
go vet ./...
```

Tests are fixture-driven for the IO-touching code paths so they don't hit
GitHub. The `probe/` directory holds a small live-API smoke harness used to
pin the go-gh signatures during initial setup; see
`internal/ghclient/SIGNATURES.md` for the verification record.
