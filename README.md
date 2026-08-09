# go-knowrag

Camada de busca semântica (RAG) sobre bases de notas em Markdown, servida a agentes de IA através de
um **MCP server escrito em Go**.

Você aponta o go-knowrag para as pastas de notas com frontmatter YAML declaradas na configuração.
Ele lê, divide por seção, gera embeddings, indexa num Qdrant e expõe uma ferramenta de busca que
qualquer cliente MCP consegue chamar. O agente passa a responder com base no seu conteúdo, citando o
trecho de origem.

Não é "aponte para qualquer pasta e funciona": os enums de frontmatter e o mapa de áreas por vault
são fechados na especificação, e pasta de primeiro nível fora do mapa é erro explícito, não `area`
vazia. Um fork adapta esses dois pontos antes de indexar suas próprias notas.

> **Status: pipeline funcionando ponta a ponta.** Ingestão, busca híbrida e o servidor MCP estão
> implementados e rodando contra um corpus real. O que falta são os modos de ingestão (`S06b`), a CLI
> de operador (`S09`) e as stories de garantia e deploy (`S10`–`S12`).
>
> Medido numa instalação real: 730 notas viram 3.647 chunks indexados; ingestão completa em ~13 min;
> busca de query em ~70 ms (p99). Um gate de performance **não** passa — reingestão incremental leva
> ~7 min contra um orçamento de 60 s, com a causa medida e a correção identificada.

> **Motivação: organização própria.** O projeto está totalmente disponível para inspiração e forks, fiquem à vontade para fazer suas cópias e alterar o que for preciso, pois ele está totalmente voltado para minha própria organização pessoal.
> Disponibilizado para compreensão de uso de RAG e para portifolio próprio.

---

## O problema

Uma base de notas grande é ótima para escrever e péssima para consultar por um agente. A busca do
editor é textual e manual; o agente não alcança nada. O conhecimento existe, está organizado, e
mesmo assim é inacessível no momento em que seria útil.

Colar as notas no prompt não resolve: não cabe, e o que cabe é escolhido a esmo. O que falta é
recuperação — trazer os poucos trechos certos, no momento da pergunta.

## O que ele faz

**Ingestão idempotente.** Lê as notas, faz chunking com fronteira em `##` (H2) e embute o breadcrumb
`H1 → H2` no texto embedado, para o trecho carregar seu próprio contexto. Rodar duas vezes converge
para o mesmo índice sobre as notas que existem: sem duplicatas e sem contagem crescente. O que
sobrou de uma nota apagada é **detectado e reportado em toda ingestão**, e removido só sob comando
explícito (`--prune`) — até lá, ela continua pesquisável.

**Busca híbrida.** Cada trecho é indexado com vetor denso *e* esparso no mesmo passo. A consulta roda
as duas buscas e funde os resultados por *Reciprocal Rank Fusion* — o denso pega paráfrase e sinônimo,
o esparso pega o termo exato, o nome próprio e a sigla que o denso dilui.

**Multi-tenancy desde o primeiro commit.** Coleções separadas por fronteira de confiança, com
`tenant_id` indexado dentro de cada uma. O filtro de tenant é decidido **pelo servidor, a partir da
sua configuração** — nunca pelo modelo, que não tem sequer um parâmetro para pedir outro tenant.

**Servido por MCP.** Um MCP server em Go, transporte stdio, sem amarração a um cliente específico —
o protocolo é padrão, então qualquer agente que fale MCP consegue consumir. O que ele enxerga, no
entanto, não é uma porta aberta: cada instância nasce com a coleção e o tenant **fixados na própria
configuração**, e quem pode consultar o quê é decisão de deploy.

**Avaliação entregue junto, não depois.** Um golden set mede Recall@5 e uma suíte adversarial testa
vazamento entre tenants — os dois como gates, não relatórios que alguém lê quando lembra. São dois
gates distintos, e a separação é proposital: o **hermético**, sobre fixture sintético versionado,
roda no CI e **bloqueia merge**; a avaliação sobre a base e o deploy reais roda em runner privado e
**bloqueia release**. Assim o CI não precisa de acesso ao corpus.

## Como funciona

```
    notas .md            ┌──────────────┐
   (frontmatter)  ─────► │   ingestão   │  chunking por ## + breadcrumb
                         │     (Go)     │  embeddings denso + esparso
                         └──────┬───────┘  upsert idempotente
                                │
                                ▼
                         ┌──────────────┐
                         │    Qdrant    │  3 coleções · tenant_id indexado
                         └──────┬───────┘
                                │
                         ┌──────┴───────┐
                         │  retrieval   │  única implementação de busca
                         │   (pacote)   │  hybrid + RRF + filtro de tenant
                         └──┬────────┬──┘
                            │        │
                   ┌────────┘        └────────┐
                   ▼                          ▼
            ┌─────────────┐            ┌─────────────┐
            │ MCP server  │            │     CLI     │
            │   (stdio)   │            │  (operação) │
            └─────────────┘            └─────────────┘
                   │
              agente de IA
```

O desenho é lógico, não de deploy: **a ingestão em lote roda numa máquina com GPU, separada**, e
escreve no Qdrant atravessando a rede; o índice, o embedding da consulta e o MCP server ficam **no
servidor**. É dessa divisão que saem duas das decisões de arquitetura do projeto.

Os dois entrypoints são **finos por regra**: toda a lógica de busca vive num único pacote, e um teste
de arquitetura no CI falha se qualquer código de busca aparecer fora dele. Não existe a versão da
busca do MCP e a versão da CLI, que divergem em seis meses.

## Funcionalidades

| | |
|---|---|
| **Chunking com contexto** | Fronteira em `##`, breadcrumb `H1 → H2` embutido, clamp de tamanho em tokens, com piso e teto a calibrar contra o corpus real |
| **Busca híbrida nativa** | Denso (1024-dim) + esparso, fundidos por RRF no próprio Qdrant |
| **Ingestão incremental** | Só reprocessa o que mudou, comparando uma fingerprint por ponto |
| **Detecção de órfãos** | Toda ingestão reporta pontos de notas apagadas ou encolhidas, mesmo sem removê-los |
| **Convergência sob falha** | Interromper a ingestão no meio nunca tira uma nota da busca — o pior caso é um ponto extra, que a rodada seguinte limpa |
| **Isolamento verificado** | Suíte adversarial pass/fail, sem meio-termo, no CI sobre fixture sintético e contra o deploy real antes do release |
| **Conteúdo marcado como não confiável** | Resultados chegam ao agente delimitados como dado, não instrução, com os delimitadores escapados no texto recuperado |
| **CLI de operação** | Ingestão, reindex, prune, avaliação e debug de busca |
| **Metadados ricos** | Tipo, status, tags, visibilidade, caminho, datas e área derivada da estrutura de pastas, gravados no payload de cada trecho |
| **Filtros de busca** | Área, tipo, vault e tags. `status: archived` fica fora por default, e `visibility: private` não sai por caminho de consumo nenhum — as duas são políticas do pacote de busca, não filtros de quem chama |

## Stack

| Componente | Escolha |
|---|---|
| Linguagem | Go |
| Vector store | Qdrant self-hosted, via gRPC |
| Embeddings | BGE-M3 (licença MIT), denso + esparso no mesmo passo |
| Protocolo | Model Context Protocol, SDK oficial em Go, transporte stdio |

O modelo de embedding é fixado por revisão imutável, e a especificação exige que a revisão seja
**confirmada pelo backend no startup** — não declarada pelo cliente. A intenção é que um container
servindo o modelo errado falhe de imediato, em vez de degradar a qualidade da busca em silêncio por
semanas. É comportamento especificado, não medido: a decisão de *como* servir o modelo (sidecar HTTP
ou ONNX no próprio processo) segue em medição, e parte dos campos do handshake ainda não tem valor
pinado.

## Design, em quatro escolhas

**A ingestão insere antes de podar, não o contrário.** Apagar os pontos de uma nota para reescrevê-los
abre uma janela em que a nota simplesmente não existe na busca — e um crash ali a deixa assim. Na
ordem invertida, o pior caso é um ponto obsoleto a mais, que a rodada seguinte remove.

**Uma fingerprint por ponto, cobrindo tudo.** Texto, metadados, config do pipeline e config confirmada
do modelo entram num único hash. Todo campo gravado no payload entra nele, sem exceção — um campo fora
do hash é um campo que uma escrita manual corrompe sem que nada detecte.

**O filtro de tenant é integridade de código, não permissão em runtime.** Nenhuma consulta sai do
pacote de retrieval sem tenant. Isso é diferente de autorização, que é decisão de deploy — e a
distinção está escrita, porque confundir as duas é como vazamento entre tenants acontece.

**Convergência, não atomicidade.** O Qdrant não garante rollback entre múltiplos pontos, então o
sistema não finge que garante. Em vez de assumir escrita atômica, cada ponto é verificado
individualmente contra quatro condições, e qualquer estado parcial converge na execução seguinte.


## Como rodar

Três processos: o serviço de embedding (Python, precisa da GPU), o Qdrant (container), e os binários
Go. O serviço de embedding é **residente** — carregar o modelo leva ~11 s, e um processo que carrega
por consulta transformaria uma busca de 70 ms em uma de onze segundos.

### 1. Qdrant

```bash
QDRANT_API_KEY=$(openssl rand -base64 32) docker compose up -d
```

O `docker-compose.yml` na raiz exige a variável e falha se ela faltar, em vez de subir um banco sem
autenticação.

### 2. Serviço de embedding

```bash
python3 -m venv ~/.venvs/knowrag && ~/.venvs/knowrag/bin/pip install FlagEmbedding
~/.venvs/knowrag/bin/python scripts/embedder-service/server.py --port 7999
```

Ele recusa subir se o modelo não normalizar os vetores ou não devolver a metade esparsa — as duas
propriedades de que o resto do pipeline depende, verificadas contra um vetor real no startup em vez
de assumidas.

Primeira execução baixa ~2,3 GB de pesos. Com cache, sobe em segundos. `--fp32` troca precisão por
VRAM; o default é fp16, que ocupa ~1,2 GB.

### 3. Provisionar o schema e ingerir

```bash
go build -o ~/bin/knowrag ./cmd/cli
knowrag schema apply                 # idempotente: rodar de novo não escreve nada
knowrag ingest --vault both --dry-run  # conta chunks sem gastar GPU nem rede
knowrag ingest --vault both
```

### 4. Servidor MCP

```bash
go build -o ~/bin/knowrag-mcp ./cmd/mcp-server
```

Transporte stdio: o cliente MCP o executa como processo filho. Não abra porta, não rode como daemon.

## Configuração

Tudo por variável de ambiente, ou por arquivo YAML apontado por `KNOWRAG_CONFIG_FILE` com as mesmas
chaves em `snake_case`. A obrigatoriedade é **por comando** — `schema apply` não exige as variáveis
do embedder, que ele nunca usa.

### CLI (`knowrag`)

| Variável | Para quê |
|---|---|
| `QDRANT_ENDPOINT` | `host:6334` — gRPC, o único protocolo que o código fala |
| `QDRANT_API_KEY` | chave do Qdrant |
| `EMBEDDER_ENDPOINT` | URL do serviço de embedding, ex.: `http://127.0.0.1:7999` |
| `DEFAULT_COLLECTION` | collection alvo |
| `LOG_LEVEL` | opcional, default `info` |
| `KNOWRAG_VAULT_<NOME>_PATH` | raiz do vault no disco |
| `KNOWRAG_VAULT_<NOME>_EXCLUDE_FOLDERS` | pastas de 1º nível ignoradas, separadas por vírgula |
| `KNOWRAG_VAULT_<NOME>_EXCLUDE_ROOT_FILES` | arquivos `.md` na raiz ignorados, separados por vírgula |

As exclusões vêm de configuração, não do código: re-incluir uma pasta excluída é uma linha de
config. Pasta de 1º nível que não está nem no mapa de áreas nem na lista de exclusão é **erro** —
excluído é decisão declarada, desconhecido é erro.

### Servidor MCP (`knowrag-mcp`)

| Variável | Para quê |
|---|---|
| `MCP_QDRANT_ENDPOINT` | `host:6334` |
| `MCP_QDRANT_API_KEY` | chave do Qdrant |
| `MCP_EMBEDDER_ENDPOINT` | URL do serviço de embedding |
| `MCP_COLLECTION` | collection consultada |
| `MCP_TENANT_ID` | tenant de toda busca |

`MCP_TENANT_ID` vem do ambiente e **não existe** como parâmetro da ferramenta. Não é validado e
rejeitado — está ausente do schema publicado, então não é um valor que o modelo possa nomear,
escrever errado, ou ser convencido a trocar por um trecho de nota hostil.

## O que suas notas precisam ter

O sistema é **fail-closed**: uma nota fora do contrato faz o scan inteiro falhar, com o arquivo e a
regra violada na mensagem. Isso é deliberado — indexar 729 de 730 notas e não avisar qual faltou é
pior do que não indexar.

### Frontmatter obrigatório

```yaml
uid: 0198a7f2-4b31-7c42-9e15-3d8a92c47b6a   # UUIDv7, forma canônica de 36 caracteres
type: concept                                # vocabulário fechado, ver abaixo
status: draft                                # draft | in-progress | stable | archived
created: 2026-08-07                          # AAAA-MM-DD, sem hora
tags: [golang, arquitetura]                  # lista não-vazia
```

Opcionais: `title`, `lang`, `visibility` (`private｜internal｜shareable`, default `internal`).

`type` aceita: `concept, moc, project, post, lesson, reference, template, log, lore, character,
script, prompt, index, agent, skill`.

O `uid` precisa ser a forma canônica exata — minúscula, com hífens. `uuid.Parse` aceita mais quatro
grafias do mesmo valor, e aceitá-las faria a identidade de uma nota depender de como alguém digitou.

### `area` vem da pasta, não do frontmatter

A **pasta de primeiro nível** define a `area`, em minúscula e sem normalização: `Research/` vira
`research`, `00-Inbox/` vira `00-inbox`. Se você escrever `area:` no frontmatter, é ignorado.

O segundo segmento do caminho vira `sub`, verbatim.

**Pasta de primeiro nível que não está no mapa de áreas nem na lista de exclusão é erro.** A regra é
*excluído é decisão declarada, desconhecido é erro* — nunca `area` vazia em silêncio. O mesmo vale
para `.md` solto na raiz do vault: ou está na lista de exclusão, ou falha.

### O acoplamento desta primeira versão

> **Os nomes de vault e o mapa de `area` por vault estão cravados em
> [`internal/schema/enums.go`](internal/schema/enums.go).** Não são configuração — são constantes de
> compilação, com um teste de arquitetura que recusa qualquer outro pacote redeclarando esses
> valores.

Isso é dívida conhecida, não desenho pretendido. `type`, `status` e `visibility` são fechados **pelo
contrato** e nenhuma instalação deveria mudá-los; nome de vault e mapa de áreas são **configuração de
instalação** e não deveriam estar ali. A separação virá; por ora, saiba onde ela não existe.

**Para adaptar a um fork, edite `internal/schema/enums.go`:**

| O que mudar | Onde |
|---|---|
| Nomes de vault (`registerVault`) | um por vault seu |
| Áreas por vault (`registerArea`) | o segundo argumento diz **em quais vaults** aquela área vale |

A validade de `area` é **por vault**: a mesma pasta pode ser válida num e desconhecida no outro, e
isso é intencional. `registerArea("00-inbox", vaultA, vaultB)` declara uma área compartilhada uma
vez; `registerArea("carreira", vaultB)` declara uma exclusiva.

Depois de editar, `go build` e o teste de arquitetura dizem se sobrou alguma lista paralela.

### Padrões que evitam dor de cabeça

- **Uma pasta de primeiro nível por área, e declare todas.** O erro mais comum é criar uma pasta nova
  no Obsidian e a ingestão parar — por desenho, para você decidir se é área ou exclusão.
- **Não use `|` num heading logo depois de uma tabela sem linha em branco.** O separador de tabela e
  o heading competem; há teste para isso, mas a linha em branco é grátis.
- **Symlinks são recusados**, arquivo e diretório. Link para fora do vault leria conteúdo de fora;
  link para diretório sumiria com as notas dele em silêncio.
- **Nota vazia é pulada e registrada**, não é erro.
- **Bloco de código ou tabela acima de 8192 tokens é erro**, não truncamento. Truncar mudaria o texto
  embedado sem mudar o texto guardado.
- **`archived` é indexado**, e filtrado na consulta — não excluído da ingestão.

## Licença

MIT — ver [`LICENSE.md`](LICENSE.md).
