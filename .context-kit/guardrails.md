# Guardrails — {project}

<!-- Project non-negotiables. These outrank everything in ~/.context-kit except Rule Zero
     (no production code without a signed Contract).
     8-12 rules. A list of 40 is a list nobody reads. Each rule must be checkable against
     a diff — "be careful with the payment code" is not a guardrail, it is a mood. -->

## Architecture

1. {e.g. Handlers never talk to the database directly; they go through a repository.}
2. {e.g. No business logic in migrations.}

## Data

3. {e.g. Money is stored in integer cents. A float touching a money value is a BLOCKER.}
4. {e.g. Every query in a multi-tenant table is scoped by tenant_id, default-deny.}

## Security

5. {e.g. No secret is read from anywhere but the environment.}
6. {e.g. User input never reaches a shell, a query string, or a template unescaped.}

## Process

7. {e.g. Migrations are reversible or ship with a written rollback procedure.}
8. {e.g. No new runtime dependency without it being named in a signed Contract.}

## Known traps in this codebase

<!-- The things that have bitten before. This section earns the file's keep. -->

9. {e.g. `LegacyOrderService` bypasses the repository layer. Do not copy its pattern.}
