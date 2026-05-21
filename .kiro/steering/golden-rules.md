---
description: Core development standards
alwaysApply: true
---

# Golden Rules

1. Backward Compatibility — all API changes must be additive.
2. Test Coverage >= 90% — unit tests for methods, integration tests for endpoints.
3. No Secrets in Code — use environment variables or secret management.
4. Structured Logging — include correlation IDs and appropriate log levels.
5. Minimal Diff — one story = one PR. Don't refactor unrelated code.
6. Input Validation — validate all user inputs, never trust client data.
7. Error Handling — return structured error responses, never expose internals.
