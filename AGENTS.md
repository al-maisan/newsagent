# AGENTS.md — conventions for agent sessions working on newsagent

## Sessions and prefixes

- Every agent session gets a number from kuma: **agent-na1**, **agent-na2**,
  **agent-na3**, … If you don't know yours, ask before opening anything.
- Sign everything you open or post with it, so kuma can tell sessions apart:
  - PR title: `agent-na3: <subject>`
  - branch: `na3/<subject>`
  - issue title, PR comments and review comments: start with `**agent-na3:**`

## Before you start work

- Product decisions are kuma's, and they are made in whichever of his
  sessions the topic comes up, so they may not be visible here. Before
  starting a feature or a new source, check the open PRs, open issues and
  recent reviews for:
  - a decision that already covers it, and
  - another session already building it.
- If unsure, ask. Never pick up work that is assigned to another session.

## Delivery

- Work in a separate git worktree (e.g. under /var/tmp), not in the
  checkout of main.
- Everything goes through a PR; never push to main.
- `go vet` and `go test -race ./...` must pass before a PR is opened.
- Documentation ships in the same PR as the code that changes it
  (README, docs/setup.md, deploy/ examples).

2026-11-24: na3 opened a Reddit-API PR an hour after kuma had decided on
the email route in another session; this file exists so that does not
happen twice.
