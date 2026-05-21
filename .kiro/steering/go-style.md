---
description: Go-specific coding conventions
globs:
  - **/*.go
---

# Go Style

- Use slog for structured logging with context.
- Return errors — don't panic in library code.
- Write table-driven tests with t.Run subtests.
- Keep functions small and focused (< 40 lines as a guide).
- Use context.Context as the first parameter for any I/O function.
- Prefer explicit over implicit — name return values only when it aids clarity.
