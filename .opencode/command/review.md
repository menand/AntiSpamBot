---
description: Полное ревью со всех сторон — архитектура, безопасность, код, тесты, продукт
---

Full multi-angle review. Scope requested by the user: $ARGUMENTS

Follow the full-review skill workflow exactly:

1. Resolve scope (`$ARGUMENTS`: empty = working-tree diff vs HEAD via `git diff HEAD` + `git status --porcelain`; explicit paths = those files; "all"/"весь проект" = whole codebase, prioritizing internal/bot pipeline files and internal/storage; "commit" = HEAD via `git show`). State the resolved scope in one line.
2. Dispatch five parallel Task tool calls in ONE message with subagent_type exactly: `architect`, `security-reviewer`, `code-reviewer`, `tester`, `product-analyst`. Each gets: repo = AntiSpam Telegram bot (Go, telego, SQLite), project law = CLAUDE.md at repo root, the resolved scope, and the instruction to read CLAUDE.md first, review only the scope, follow its own checklist, and answer in its strict findings format.
3. Merge into a single Russian-language report: **Итог** (verdict + counts), **Таблица находок** (Severity | файл:строка | суть | кто поймал; dedupe overlaps), **Топ-5 приоритетов**, **Тесты** (PASS/FAIL matrix verbatim), **Спорное** (за/против). No emojis. Cite only file:line the reviewers produced.
