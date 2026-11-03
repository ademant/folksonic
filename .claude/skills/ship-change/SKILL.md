---
name: ship-change
description: The end-to-end workflow for changing tinysonic/folksonic code in this repo — proposal, GitHub issue, branch, implementation with tests, verification, commit, push, pull request and merge. Use whenever the user asks to implement, fix, add or change something in the code, says "create issue", "commit", "push", "open PR" or "merge PR #N", or a diagnosed bug needs fixing. Complements CLAUDE.md with the practical details of this machine.
---

# Shipping a change

CLAUDE.md defines the rules (discuss → issue → implement → commit with `Closes #N`). This skill adds what it takes to follow them on this machine.

## 1. Proposal

For anything beyond a typo or one-line fix, present the plan before touching code: what exists already (look it up), the design, tests, docs, and the **decisions the user has to make**, each with a recommendation. Flag conflicts with project principles explicitly (e.g. a new external dependency vs "pure Go"). Wait for an explicit OK on that proposal; the answer often only settles some decisions — follow the recommendation for the rest and say so.

## 2. Issue and branch

```bash
git checkout main && git pull
git checkout -b <feat|fix|chore>/<short-name>
gh issue create --repo ademant/folksonic --title "…" --body "## Problem … ## Solution … ## Impact …"
```
Record the user's decisions in the issue body. If the user only asked for the issue, stop there.

## 3. Implement

- Read the current code first; files change between sessions (other branches get merged).
- For a bug fix, write the regression test from the real failing path and **show it fails without the fix** (temporarily restore the old file, run the test, restore the fix). Quote the failure; it proves the test catches the bug.
- Schema changes need an idempotent step in `migrate()`; think about how long it takes on the production database (5k+ songs, FTS triggers).
- Update README (and CLAUDE.md layout table if files moved) in the same change.

## 4. Verify

`make check` runs build, vet, gofmt and tests with the low-memory Go flags this 4 GB host needs. Plain `go build/test` outside make needs them exported:
```bash
export GOGC=20 GOFLAGS='-p=1 -gcflags=all=-c=1'
```
For user-facing flows, also smoke-test a real binary on a temporary config/database (separate port, `--no-scan --no-watch`). Report anything you could not check (e.g. browser JavaScript).

## 5. Commit — only when asked

Git has no global identity here. Commit as the user, per command, without changing git config:
```bash
git -c user.name=ademant -c user.email=<user's email> commit -F - <<'EOF'
<type>: <summary>

<why and what>

Closes #N

Co-Authored-By: <attribution line from the current system reminder>
EOF
```
One `Closes #N` line per issue.

## 6. Push and PR — only when asked

```bash
git push -u origin <branch>
gh pr create --repo ademant/folksonic --base main --head <branch> --title "…" --body "Closes #N … ## Testing …"
```
The Testing section lists what was run and what was not. End the body with the PR attribution line from the current system reminder.

## 7. Merge — only when asked

The `gh` token cannot merge PRs ("Resource not accessible by personal access token"). Merge locally with the same commit GitHub would create, then confirm:
```bash
git checkout main && git pull
git -c user.name=ademant -c user.email=<user's email> merge --no-ff <branch> -m "Merge pull request #<PR> from ademant/<branch>"
git push origin main
gh pr view <PR> --json state; gh issue view <N> --json state
```

If production runs a branch build, say that `main` lacks the fix until merged; after merging, deploying `main` is safe again (prod-deploy skill).
