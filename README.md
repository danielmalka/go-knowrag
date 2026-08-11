# go-knowrag

Camada de busca semântica (RAG) sobre bases de notas em Markdown, servida a agentes de IA através de
um **MCP server escrito em Go**.

Você aponta o go-knowrag para as pastas de notas com frontmatter YAML declaradas na configuração.
Ele lê, divide por seção, gera embeddings, indexa num Qdrant e expõe uma ferramenta de busca que
qualquer cliente MCP consegue chamar. O agente passa a responder com base no seu conteúdo, citando o
trecho de origem.

Não é "aponte para qualquer pasta e funciona": os enums de frontmatter (`type`, `status`,
`visibility`) são fechados no contrato, e pasta de primeiro nível fora da lista de áreas configurada
é erro explícito, não `area` vazia. Um fork aponta para suas próprias pastas e declara seus próprios
vaults e áreas em configuração, sem editar Go — mas os **nomes das coleções** ainda não: eles são
literais compilados em `internal/schema/manifest.go`, e trocá-los é editar Go.

> **Status: pipeline funcionando ponta a ponta, com os dois gates de performance passando.**
> Ingestão, busca híbrida e o servidor MCP estão implementados e rodando contra um corpus real.
>
> Medido numa instalação real em 2026-08-11: 735 notas viram 3.690 pontos indexados; ingestão
> completa em **6m50s** contra um orçamento de 30 min, e reingestão de um corpus inalterado em
> **30,9 s** contra um orçamento de 60 s. Esse segundo número já foi 403 s: a causa não era o
> chunking, como se supunha, e sim uma interação entre o algoritmo de Nagle e o delayed-ACK do Linux
> numa conexão reusada com o serviço de embedding — resolvida com um `TCP_NODELAY` do lado Python.
> Busca de query em ~70 ms (p99).
>
> Falta a CLI de operador (`S09`), parte dos modos de ingestão (`S06b` — o lock de execução
> concorrente já entrou; `--only` e `--prune` não) e as stories de garantia e deploy (`S10`–`S12`).

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
para o mesmo índice sobre as notas que existem: sem duplicatas e sem contagem crescente. A cauda de
uma nota que **encolheu** é removida sozinha, em toda ingestão, logo depois do upsert confirmado.
Os pontos de uma nota **apagada** não: nada compara o que está indexado com o que foi varrido, então
eles ficam no índice sem detecção e sem aviso, e continuam saindo na busca.

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

**Avaliação: o desenho existe, a entrega não.** O plano é um golden set medindo Recall@5 e uma suíte
adversarial de vazamento entre tenants, os dois como gates e não como relatório que alguém lê quando
lembra — o **hermético**, sobre fixture sintético versionado, bloqueando merge no CI; o que roda
contra a base e o deploy reais, em runner privado, bloqueando release. Assim o CI nunca precisaria de
acesso ao corpus.

Nada disso está construído. `internal/eval/` tem só um `doc.go`, e os dois jobs correspondentes no
`.github/workflows/ci.yml` estão `if: false` com um `echo` no lugar do comando (`S10`, `S11`). **O
que bloqueia merge hoje é o lint e a suíte unitária, esta com `-race`.** O único teste de isolamento
entre tenants que existe está atrás da tag `integration` (`internal/retrieval/integration_test.go`):
roda à mão no runner privado, nunca no CI e nunca contra uma PR.

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

Os dois entrypoints são **finos por convenção**: toda a lógica de busca vive num único pacote, para
não existirem a versão da busca do MCP e a versão da CLI, que divergem em seis meses. O teste de
arquitetura no CI (`internal/archtest/boundary_test.go`) trava uma fronteira vizinha, e só ela:
**nenhum pacote fora de `internal/store` importa o cliente do Qdrant**. Código de busca escrito fora
do pacote de retrieval é pego por revisão, não por teste.

## Funcionalidades

| | |
|---|---|
| **Chunking com contexto** | Fronteira em `##`, breadcrumb `H1 → H2` embutido, clamp de tamanho em tokens, com piso e teto a calibrar contra o corpus real |
| **Busca híbrida nativa** | Denso (1024-dim) + esparso, fundidos por RRF no próprio Qdrant |
| **Ingestão incremental** | Só reprocessa o que mudou, comparando uma fingerprint por ponto |
| **Poda da cauda** | A cauda de uma nota que encolheu é removida em toda ingestão, depois do upsert confirmado. Ponto de nota **apagada** não é detectado nem reportado — fica no índice |
| **Convergência sob falha** | Interromper a ingestão no meio nunca tira uma nota da busca — o pior caso é um ponto extra, que a rodada seguinte limpa |
| **Isolamento por integridade de código** | Nenhuma consulta sai do pacote de retrieval sem `tenant_id`. A suíte adversarial que verificaria isso como gate está especificada e não existe |
| **Conteúdo marcado como não confiável** | Resultados chegam ao agente delimitados como dado, não instrução, com os delimitadores escapados no texto recuperado |
| **CLI de operação** | Dois comandos: `schema apply` e `ingest`. Reindex, prune, avaliação e debug de busca não existem |
| **Metadados ricos** | Tipo, status, tags, visibilidade, caminho, datas e área derivada da estrutura de pastas, gravados no payload de cada trecho |
| **Filtros de busca** | A ferramenta MCP expõe `area` e `type` (mais `query` e `top_k`). `vault` e `tags` existem só na API Go: nenhum caminho entregue chega neles. `status: archived` fica fora por default, e `visibility: private` não sai por caminho de consumo nenhum — as duas são políticas do pacote de busca, não filtros de quem chama |

## Stack

| Componente | Escolha |
|---|---|
| Linguagem | Go |
| Vector store | Qdrant self-hosted, via gRPC |
| Embeddings | BGE-M3 (licença MIT), denso + esparso no mesmo passo |
| Protocolo | Model Context Protocol, SDK oficial em Go, transporte stdio |

O modelo de embedding é fixado por revisão imutável, e a revisão é **confirmada pelo backend** — não
declarada pelo cliente. Um serviço rodando o modelo errado precisa falhar de imediato, em vez de
degradar a qualidade da busca em silêncio por semanas.

Isso está implementado e rodando. Os sete campos da config efetiva têm valor pinado
(`internal/embed/handshake.go`) — revisão do modelo, revisão do tokenizer, dimensão, normalização,
pooling, precisão e os parâmetros esparsos — e a ingestão aborta nomeando o campo que divergir. A
forma de servir o modelo também foi decidida e medida: sidecar HTTP, um runtime só, no host de
ingestão.

Os dois lados confirmam. A ingestão confere antes da primeira escrita; o servidor MCP confere na
subida e **recusa subir** se o backend responder com uma configuração diferente da que o índice foi
construído — porque embedar query num espaço vetorial diferente do que está armazenado não devolve
erro, devolve resultado plausível e errado, que é a única falha que quem chama não tem como
desconfiar.

Backend **fora do ar** é outra coisa e não impede a subida: o servidor sobe, avisa no log que não
conseguiu conferir, e cada busca responde dizendo que a base está indisponível. Derrubar o servidor
por causa de uma queda deixaria o cliente adivinhando.

Uma conferência pulada não vira consentimento permanente. A garantia mora no embedder, não em quem o
constrói: **nada é embedado através de um backend que aquele embedder ainda não conferiu**. Se a
checagem da subida foi pulada porque o serviço estava fora, a primeira busca que chegar faz a
conferência antes de embedar — e recusa, nomeando o campo, se o serviço tiver voltado com outra
configuração. Não é preciso reiniciar nada. Depois de uma conferência bem-sucedida ela não se repete;
o custo em regime é uma leitura atômica por chamada.

> **O que continua não coberto:** um backend que troque de revisão *depois* de uma conferência
> bem-sucedida, sem reiniciar nada dos dois lados. Fechar isso exige um token de geração no protocolo,
> que nenhum dos lados tem hoje — e é a mesma janela de qualquer cliente longevo de um serviço mutável.

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

Duas exigências do host de ingestão que abortam a rodada inteira se faltarem, e que não são
processos:

- **`git` no PATH.** O `updated` de cada nota sai de um `git log` por vault; sem o binário, o scan
  falha e nenhuma nota é indexada. Uma raiz que não é repositório git é caso previsto e cai para o
  mtime — não ter o `git` instalado, não.
- **O diretório de cache do usuário num sistema de arquivos onde `flock` exclui.** É onde mora o
  lock de ingestão; 9p, NFS e FUSE são **recusados com erro**, porque ali o lock não excluiria nada e
  daria a impressão de proteger. No WSL isso pega quem tem o `HOME` sob `/mnt/c`, que é 9p.

### 1. Qdrant

```bash
QDRANT_API_KEY=$(openssl rand -base64 32) docker compose up -d
```

O `docker-compose.yml` na raiz exige a variável e falha se ela faltar, em vez de subir um banco sem
autenticação.

**Esse arquivo é de desenvolvimento local.** O Qdrant implantado tem o seu, em
`deploy/docker-compose.yml`: mesma imagem pinada, sem porta em interface pública — o endereço de bind
vem de `deploy/.env`, documentado em `deploy/.env.example`. `make verify-deploy` confere pino de
imagem, endereço de bind e credencial obrigatória lendo o arquivo, sem precisar de Docker nem da
máquina implantada, e o CI roda isso ao lado do linter.

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

O serviço é residente e nada o traz de volta sozinho depois de um reboot ou de um `kill`. Para deixar
isso com o init em vez de com você, há um unit de usuário do systemd em
`scripts/embedder-service/`:

```bash
mkdir -p ~/.config/knowrag ~/.config/systemd/user
cp scripts/embedder-service/knowrag-embedder.env.example ~/.config/knowrag/embedder.env
$EDITOR ~/.config/knowrag/embedder.env          # os caminhos desta máquina moram aqui, não no unit
cp scripts/embedder-service/knowrag-embedder.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now knowrag-embedder
loginctl enable-linger "$USER"                  # sem isto ele morre junto com a sessão
```

`journalctl --user -u knowrag-embedder -f` mostra o startup, inclusive a recusa de subir quando o
modelo falha na verificação. Depois de instalar, vale confirmar que o restart funciona em vez de
confiar no arquivo: `kill -9` no PID do serviço e checar que `/health` volta a responder sozinho.

### 3. Provisionar o schema e ingerir

```bash
go build -o ~/bin/knowrag ./cmd/cli
knowrag schema apply                 # idempotente: rodar de novo não escreve nada
knowrag ingest --vault both --dry-run  # conta chunks sem gastar GPU nem escrever no Qdrant
knowrag ingest --vault both
```

`--dry-run` não é offline: o clamp conta tokens reais do BGE-M3 e se recusa a aproximar, então ele
exige `EMBEDDER_ENDPOINT` e faz um `POST /tokenize` por chunk. O que ele economiza é a GPU e a
escrita — uma contagem tirada de um contador aproximado reportaria um número de chunks que a rodada
real não produz.

`schema apply` também mantém um registro do que já foi aplicado, em
`internal/schema/applied_state.json`, **resolvido a partir do diretório de trabalho**. Rodando o
binário de fora do repositório, aponte `--state-file` para esse arquivo: sem registro, a deriva de
revisão do modelo de embedding é a única deriva que deixa de ser detectada, em silêncio.

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
| `KNOWRAG_ADMIN_QDRANT_API_KEY` | chave **administrativa** do Qdrant — a CLI aceita qualquer `--tenant` e qualquer `--collection`, então a credencial dela é a do operador. O servidor MCP lê `MCP_QDRANT_API_KEY` e nenhuma das duas cai para a outra |
| `EMBEDDER_ENDPOINT` | URL do serviço de embedding, ex.: `http://127.0.0.1:7999` |
| `DEFAULT_COLLECTION` | collection alvo |
| `LOG_LEVEL` | opcional, default `info` |
| `KNOWRAG_VAULTS` | os vaults desta instalação, separados por vírgula — ex.: `pessoal,trabalho` |
| `KNOWRAG_VAULT_<NOME>_PATH` | raiz do vault no disco |
| `KNOWRAG_VAULT_<NOME>_AREAS` | pastas de 1º nível que são `area` válida neste vault, separadas por vírgula |
| `KNOWRAG_VAULT_<NOME>_EXCLUDE_FOLDERS` | pastas de 1º nível ignoradas, separadas por vírgula |
| `KNOWRAG_VAULT_<NOME>_EXCLUDE_ROOT_FILES` | arquivos `.md` na raiz ignorados, separados por vírgula |

`KNOWRAG_VAULTS` é a lista mestra: nada é lido de um vault que não está nela. Uma variável
`KNOWRAG_VAULT_*` de um vault fora da lista é **erro** — deixada em silêncio, aquele vault
simplesmente nunca seria indexado e os pontos dele envelheceriam sem aviso.

**`<NOME>` é o nome do vault em maiúsculas, com `-` virando `_`.** Um vault chamado `my-notes` lê
`KNOWRAG_VAULT_MY_NOTES_PATH`; não existe outra grafia dele.

**Nome de vault e nome de área têm de ser *slug* ASCII minúsculo** — `^[a-z0-9]+(-[a-z0-9]+)*$`:
sem maiúsculas, sem espaços, sem acentos, sem `_`, sem hífen no início, no fim ou dobrado. Um nome
fora dessa forma é recusado no carregamento da configuração, antes de qualquer trabalho, e **não é
normalizado**: `vault` e `area` são gravados literalmente no payload e no `point_hash`, então
minúscular um nome por baixo do operador moveria todos os hashes — e como o ID do ponto não inclui
`vault`, os pontos antigos seriam **sobrescritos**, não órfãos. Seria uma reindexação completa
reportada como execução limpa.

**Um vault chamado `both` recusa toda ingestão, não só a ambígua.** `both` é slug legal e é também a
palavra de `--vault` para "todos os vaults"; com ele no roster, não há como selecionar aquele vault
sozinho nem como dizer "todos". `knowrag ingest` recusa a rodada até o nome mudar — inclusive as
rodadas que pedem um vault qualquer por nome, porque a colisão é checada antes de o flag ser lido.

No arquivo YAML os vaults ficam num mapa aninhado sob `vaults:`, com as mesmas chaves em
`snake_case` (`path`, `areas`, `exclude_folders`, `exclude_root_files`).

As áreas e as exclusões vêm de configuração, não do código: re-incluir uma pasta excluída é uma
linha de config. Pasta de 1º nível que não está nem em `AREAS` nem na lista de exclusão é **erro** —
excluído é decisão declarada, desconhecido é erro.

### Flags de `knowrag ingest`

| Flag | Default | Para quê |
|---|---|---|
| `--vault` | `both` | um nome de `KNOWRAG_VAULTS`, ou `both` para todos |
| `--dry-run` | desligado | varre e faz chunking, e para: nem embeda nem escreve |
| `--tenant` | `interno` | o `tenant_id` sob o qual todo ponto é escrito |
| `--floor-tokens` | `256` | junta seções irmãs consecutivas abaixo desse tamanho |
| `--ceiling-tokens` | `1024` | quebra a seção acima desse tamanho |

**Os três últimos entram no `point_hash`, e `--tenant` entra também no ID do ponto.** Mudar qualquer
um deles não ajusta a próxima rodada: faz o índice inteiro deixar de bater e ser reescrito. Os
valores de piso e teto são o ponto de partida do contrato, não um ótimo medido — a calibração contra
o corpus real ainda não aconteceu.

**`MCP_TENANT_ID` tem de ser exatamente o `--tenant` com que a coleção foi ingerida.** Divergentes,
a busca filtra por um tenant que não tem ponto nenhum: zero resultados, sem erro, sem aviso. Do lado
da ingestão o valor tem default (`interno`) e do lado do MCP é obrigatório — então é a variável do
servidor que precisa ser escrita à mão para casar com um `--tenant` que talvez ninguém tenha digitado.

### Flags de `knowrag search`

```bash
knowrag search "como rotacionar certificados" --tenant interno
knowrag search "certificados" --tenant interno --area infra --json
```

| Flag | Default | Para quê |
|---|---|---|
| `--tenant` | **obrigatória** | o `tenant_id` de toda a busca; não existe valor que signifique "todos" |
| `--collection` | `DEFAULT_COLLECTION` | a collection consultada |
| `--area` | vazio | restringe a uma área; vazio busca todas |
| `--top-k` | `5` | quantos chunks voltam |
| `--include-archived` | desligado | inclui notas com `status: archived` |
| `--include-private` | desligado | inclui notas com `visibility: private` |
| `--json` | desligado | escreve o envelope JSON no stdout e mais nada |

**`--include-private` é o caminho privilegiado, e é o único que existe.** A tool MCP não tem campo
equivalente e estruturalmente não consegue pedir conteúdo privado — é por isso que ele mora aqui e
por isso que a CLI usa credencial administrativa própria. A paridade entre esta busca e a do
servidor MCP é definida com esta flag e `--include-archived` desligadas, e é verificada em cada
`go test` por `cmd/mcp-server/search_parity_test.go`, sem rede e sem Qdrant.

Exit codes, válidos para toda a CLI: `0` ok, `2` erro de uso (o comando precisa mudar), `1` falha de
backend (vale tentar de novo), `3` outra ingestão segurando o lock, `4` gate que rodou e reprovou,
`130` interrompido. O `4` é separado do `1` de propósito: recall que caiu é uma medição verdadeira de
um sistema pior, e quem lê `1` volta a tentar.

### `knowrag stats`

```bash
knowrag stats                      # todo tenant, todas as collections
knowrag stats --tenant interno --json
```

Por collection, quantos pontos e quantas notas distintas. **A diferença entre os dois números é onde
o órfão aparece**: uma nota que encolheu deixa pontos para trás que não pertencem mais a chunk nenhum.
Para contar uid distinto ele lê o uid de cada ponto — uma passada inteira pela collection, alguns
segundos contra o Qdrant implantado. É comando para rodar de propósito, não em cron.

Escopo deliberadamente mínimo (PRD): serve para conferir ingestão e detectar órfão, não é
observabilidade. Não tem flag além de `--tenant` e `--json`.

### `knowrag eval`

```bash
knowrag eval --golden      # recall contra o golden set
knowrag eval --isolation   # a suíte de isolamento entre tenants
```

Exatamente um dos dois modos, obrigatório. **Nenhum dos dois tem harness ainda** — S10 constrói o
golden set, S11 a suíte de isolamento — e o comando recusa dizendo qual história falta em vez de
reportar um passe que ninguém mediu. Os dois jobs de CI rodam este comando em toda push e reportam
*pendente*; o dia em que o harness existir, eles viram gate sem ninguém ligar nada
(`scripts/ci/eval-gate.sh`).

### Servidor MCP (`knowrag-mcp`)

| Variável | Para quê |
|---|---|
| `MCP_QDRANT_ENDPOINT` | `host:6334` |
| `MCP_QDRANT_API_KEY` | chave do Qdrant |
| `MCP_EMBEDDER_ENDPOINT` | URL do serviço de embedding |
| `MCP_COLLECTION` | collection consultada |
| `MCP_TENANT_ID` | tenant de toda busca |
| `MCP_AREAS` | lista de `area` válida, separada por vírgula, que o servidor anuncia ao cliente MCP |

`MCP_TENANT_ID` vem do ambiente e **não existe** como parâmetro da ferramenta. Não é validado e
rejeitado — está ausente do schema publicado, então não é um valor que o modelo possa nomear,
escrever errado, ou ser convencido a trocar por um trecho de nota hostil.

`MCP_AREAS` não espelha `KNOWRAG_VAULT_<NOME>_AREAS`: uma instância MCP serve uma coleção, que pode
ter sido montada a partir de vários vaults, então a lista que ela anuncia é declarada de novo, à
parte, na configuração do servidor.

> **Essa lista é manutenção manual, mas envelhecer nela não falha mais em silêncio.** Nada compara
> `MCP_AREAS` com o índice na partida — um servidor que checasse isso no boot não subiria com o
> Qdrant fora do ar. Uma área acrescentada a um vault e esquecida aqui continua não sendo oferecida
> ao cliente. Uma área listada aqui é aceita como filtro; se a busca volta vazia, o servidor testa a
> área sozinha — sem o `type` que a busca possa ter carregado junto, porque só a área é o que ele vai
> acusar — e, quando ela não casa nada visível no índice, responde dizendo que o vazio veio do filtro
> e não do assunto, e nomeia esta variável para o operador. Ao acrescentar ou renomear uma área em
> qualquer vault, atualize `MCP_AREAS` na mesma passada.

## Quando a ingestão acontece

**Só quando você roda o comando.** Não há gatilho, watcher de sistema de arquivos, daemon nem cron:
`knowrag ingest` é um processo que começa, faz o trabalho e termina.

Isso é escolha, não lacuna a ser tapada com um `while true`. A ingestão lê o vault inteiro, fala com
um serviço de embedding e escreve num banco remoto; um watcher disparando a cada salvamento no
Obsidian faria isso dezenas de vezes por sessão de escrita, e reindexar durante a edição gasta
GPU e rede para indexar texto que ainda vai mudar.

**Rodar de novo é seguro e converge.** Cada nota carrega uma fingerprint (`point_hash`) que cobre o
texto do chunk, os metadados, a config do pipeline e a config confirmada do embedder. Uma nota cujos
pontos batem é pulada sem escrever nada, então a segunda execução sobre um vault inalterado não
reembeda — apenas verifica. A consequência prática é que **você não precisa saber o que mudou**:
rode o comando e ele descobre. Verificar 735 notas inalteradas custou 30,9 s na instalação medida: o
estado indexado do tenant é lido de uma vez, num snapshot só, e não por uma consulta por nota.

**O caminho normal é rodar quando fizer sentido para você** — depois de uma sessão de escrita, antes
de uma pesquisa importante, ou por um agendador seu (`cron`, `systemd timer`, Task Scheduler)
chamando o mesmo binário. O projeto não instala agendador nenhum, de propósito: quem sabe a cadência
certa é quem escreve as notas.

**Duas ingestões simultâneas não se atropelam mais.** Antes de ler o vault, `knowrag ingest` toma um
lock local do sistema operacional (`flock`) identificado por endpoint do Qdrant + collection +
tenant. A segunda execução no mesmo escopo é recusada na hora, com **código de saída 3** — separado
do `1` genérico justamente para um agendador distinguir "a anterior ainda está rodando" de "quebrou".
Nada é escrito e nada é lido do vault nessa recusa. `--dry-run` não toma lock: ele não escreve e nem
chega a abrir conexão com o Qdrant.

O lock é liberado pelo kernel quando o processo morre, qualquer que seja a causa — matar uma
ingestão travada não deixa nada para limpar à mão.

> **Ele não atravessa máquinas.** É um lock local. Qualquer host com o binário, a credencial e rota
> até o Qdrant consegue rodar uma segunda ingestão sobre o mesmo escopo, e nada no sistema impede.
> A topologia prevista tem um executor só; isso é um acordo, não uma garantia técnica.

Não existe ainda ingestão parcial (`--only <glob>`) nem remoção de órfãos sob comando (`--prune`) —
os dois estão especificados e ainda não implementados.

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
