# Napkin Runbook

## Curation Rules
- Re-prioritize on every read.
- Keep recurring, high-value notes only.
- Max 10 items per category.
- Each item includes date + "Do instead".

## Domain Behavior Guardrails
1. **[2026-08-16] Embedder não é 24 h**
   Do instead: use `knowrag-*.timer`. Never `enable` `knowrag-embedder.service`. Night ingest 03:00 (stops the model). Day incremental 12:00 and 18:00 (leaves it up). Query window 07:00–23:00.

2. **[2026-08-16] Busca sem o modelo é outro produto (D-41)**
   Do instead: do not add a second encoder or idle-start inside the 10 s MCP deadline. Lexical fallback needs an ADR and worse-recall acceptance.

3. **[2026-08-13] Golden set real não bloqueia entrega**
   Do instead: do not list 60–90 questions as a required next step. The runner already exists; the owner authors if they want.

4. **[2026-08-13] Endurecimento de rede está fechado**
   Do instead: do not propose ACL-by-destination or splitting the ingest admin key. Reopens only if a second untrusted machine joins the network. MCP read-only key is already the credential-scope answer.

5. **[2026-08-11] Pastas excluídas da ingestão são conteúdo, não lint**
   Do instead: do not propose re-including `PowerAI` / `resources` / `templates` because frontmatter is now valid.

6. **[2026-08-10] `docs/` fica fora do git**
   Do instead: never propose versioning, mirroring, or copying `docs/` into tracked files.

## Execution & Validation
1. **[2026-08-11] Comentário que afirma número de outro arquivo envelhece**
   Do instead: if the number lives in another file, name that file in the comment. Budget tests must sum every leg of the real path.

2. **[2026-08-13] Plante contra script/YAML/fixture precisa de `-count=1`**
   Do instead: disable Go test cache and assert the plant applied before running. A comment that quotes the target string is the wrong plant target.

3. **[2026-08-11] Antes de tratar débito como aberto, leia o código de hoje**
   Do instead: confirm the claimed failure still exists. Owner decision closes the item; update the stale document.

## User Directives
1. **[2026-08-08] Repo público**
   Do instead: never write vault names, home paths, owner email, or private IPs into tracked files. Env + `.example` only.
