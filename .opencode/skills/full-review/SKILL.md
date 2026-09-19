---
name: full-review
description: Full multi-angle review of this AntiSpam Telegram bot — dispatches five specialist subagents (architect, security-reviewer, code-reviewer, tester, product-analyst) in parallel and merges their outputs into one severity-ranked report. Use when the user says "полное ревью", "ревью со всех сторон", "проверь со всех сторон", "полный обзор качества", "full review", "comprehensive review", "review everything", or wants architecture/security/code/tests/product checked together.
---

# Full Review Orchestrator

Run a complete quality pass over a scope (diff, commit, files, or whole project) using five read-only specialist subagents, then merge results.

## 1. Resolve the scope

From the user's words (or `$ARGUMENTS` when invoked as `/review`):

- empty / «текущие изменения» → working tree: `git diff HEAD --stat` + `git status --porcelain`, then the full `git diff HEAD`; include untracked source files.
- «последний коммит» / commit → `git show --stat HEAD` + its patch.
- explicit paths → those files/dirs.
- «весь проект» / all → whole codebase; tell reviewers to prioritize `internal/bot` pipeline files (`bot.go`, `handlers.go`, `actions.go`, `spamcheck.go`, `menu.go`) and `internal/storage` (`db.go`, `schema.sql`, `migrate.go`).

State the resolved scope to the user in one line before dispatching.

## 2. Dispatch five reviews IN PARALLEL

In ONE message, make five Task tool calls with `subagent_type` exactly: `architect`, `security-reviewer`, `code-reviewer`, `tester`, `product-analyst`. Give EACH the same preamble:

- Repo: AntiSpam Telegram bot (Go, telego, SQLite). Project law: CLAUDE.md at repo root.
- Scope under review: <paste resolved scope: changed files + brief what/why>
- Instruction: read CLAUDE.md first, review ONLY the scope (read surrounding code as needed for context), follow your own checklist, answer in YOUR strict output format.

Never run them sequentially and never merge into one mega-task — separate contexts keep the perspectives independent.

## 3. Merge into one report

Deduplicate overlapping findings (same location+issue from several reviewers → one entry noting who flagged it; disagreements → «Спорное»). Produce, in RUSSIAN:

1. **Итог** — 1–2 lines: overall verdict + counts (X critical / Y major / Z minor).
2. **Таблица находок** — Severity | Где (файл:строка) | Суть | Кто поймал — sorted CRITICAL → MAJOR → MINOR.
3. **Топ-5 приоритетов** — ordered fix-now list, one-line rationale each.
4. **Тесты** — the tester's PASS/FAIL matrix verbatim + proposed tests worth adding.
5. **Спорное** — contested items with за/против, no forced verdict.

Rules: findings must cite file:line from reviewers' outputs (do not invent locations); keep reviewer wording, translate framing to Russian; no emojis; if everything is clean say so plainly instead of padding. End by offering to fix the criticals.
