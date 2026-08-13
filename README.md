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
> Ingestão, busca híbrida, CLI de operador e o servidor MCP estão implementados e rodando contra um
> corpus real.
>
> Medido numa instalação real em 2026-08-11: 735 notas viram 3.690 pontos indexados; ingestão
> completa em **6m50s** contra um orçamento de 30 min. A reingestão de um corpus inalterado já foi
> 403 s: a causa não era o chunking, como se supunha, e sim uma interação entre o algoritmo de Nagle
> e o delayed-ACK do Linux numa conexão reusada com o serviço de embedding — resolvida com um
> `TCP_NODELAY` do lado Python.
>
> Remedido em 2026-08-13 pelas duas ferramentas de medição que hoje moram no repositório
> (`cmd/measure-ingest`, `cmd/measure-search`), contra o deploy real: reingestão no-op do comando
> completo do operador em **33,7 s** contra o teto de 60 s (NFR-5), e busca com **p95 de 241 ms**
> contra o teto de 3 s (NFR-1b). Números de um deploy só, produzidos por comando de operador — não
> são benchmark deste projeto para a sua máquina.
>
> O que falta é conteúdo, não código: o golden set real — as perguntas que o `knowrag golden` existe
> para colher — ainda não foi escrito, então **não há Recall@5 medido contra a base real**. O
> instrumento inteiro existe e roda; o que falta é a pergunta, e ela só o dono pode escrever.

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
Os pontos de uma nota **apagada** são detectados e listados em toda rodada, e removidos só quando
alguém pede — `--prune`, que é destrutivo e por isso opt-in. Ver órfão é o default; apagar não é.

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

**Avaliação: os dois instrumentos existem e bloqueiam merge.** O plano é um golden set medindo
Recall@5 e uma suíte adversarial de vazamento entre tenants, os dois como gates e não como relatório
que alguém lê quando lembra — o **hermético**, sobre fixture sintético versionado, bloqueando merge
no CI; o que roda contra a base e o deploy reais, em runner privado, bloqueando release. Assim o CI
nunca precisa de acesso ao corpus.

O lado do **recall está construído e ligado**: `internal/eval/` tem o loader do golden set, o runner
determinístico, o intervalo de Wilson a 95% e o relatório, e o job hermético do CI roda
`knowrag eval --golden` de verdade contra o fixture sintético em `testdata/eval/hermetic/` a cada
push — sem Qdrant, sem embedder e sem GPU. Um recall abaixo do limiar reprova o job.

**Duas coisas, checadas em dois passos do mesmo job.** O *limiar* é um piso — recall ≥
`--min-recall` — e não uma igualdade, porque é isso que um gate de recall significa. O *fixture* é
checado à parte, porque o piso não o enxerga: apagar as perguntas deliberadamente sem resposta
levaria o recall a 1.0, o que passa o piso. Quem exige 6/8 exatos, e confere o número contra o
`ci.yml`, é `TestHermeticGoldenGate_FixtureCorpus_AchievesExpectedRecall` — rodado pelo próprio job,
não só pela suíte unitária, para que um check verde chamado `eval-golden-hermetic` signifique o que
o nome diz sem depender de outro job existir.

**A suíte de isolamento também mede, e também bloqueia merge.** `knowrag eval --isolation` roda dez
casos adversariais (`internal/eval/isolation/`) — vazamento entre tenants, tenant forjado no payload,
injeção pelo texto da consulta, `private` e `archived`, o caminho privilegiado da CLI, o caminho de
**escrita**, a amarração de escopo do servidor MCP e a fronteira de arquitetura. Ela constrói o
próprio corpus e dirige o `internal/retrieval` de verdade contra ele, então não abre conexão, não
precisa de embedder e não lê vault nenhum: o job `eval-isolation-hermetic` roda a cada push. Não tem
nota nem score — um caso reprovado reprova a suíte, porque uma suíte de isolamento com nota é uma
suíte que aceita vazar um pouco.

**Hoje bloqueiam merge: lint, `make verify-deploy`, a suíte unitária com `-race`, o gate hermético de
recall e o de isolamento.** Além deles existe um teste de isolamento atrás da tag `integration`
(`internal/retrieval/integration_test.go`), que roda contra o Qdrant real no runner privado — nunca
no CI e nunca contra uma PR.

O que falta é **o número que importa**: o golden set real. As 60–90 perguntas contra a base real não
existem, então não há Recall@5 medido deste deploy, e a escolha entre busca híbrida e densa continua
sustentada por raciocínio e não por medição. Isso é conteúdo que só o dono produz — a pergunta
precisa ser dele, e precisa ser escrita **antes** de qualquer resultado ser visto. É para isso que
existe o [`knowrag golden`](#knowrag-golden).

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
arquitetura no CI (`internal/archtest/boundary_test.go`) trava duas fronteiras vizinhas: **nenhum
pacote fora de `internal/store` importa o cliente do Qdrant**, e **o pacote de autoria do golden set
não alcança o índice** (a segunda está explicada [mais abaixo](#knowrag-golden)). Código de busca
escrito fora do pacote de retrieval, esse sim, é pego por revisão e não por teste.

As duas são checadas sobre o **grafo de imports derivado do fonte**, não sobre uma lista de pastas
escrita à mão. A diferença não é estilo: a versão anterior da segunda andava por um mapa de pacotes,
e duas revisões a desmontaram por pontas opostas — uma apagou uma linha do mapa e a invariante
desligou inteira com a suíte verde; a outra atacou o fecho transitivo que o mapa não percorria e
achou dois buracos vivos, cada um compilando e passando. Lista que alguém mantém é lista que alguém
esquece; fecho transitivo derivado a cada rodada não tem linha para apagar. Cada um dos dois testes
carrega o próprio teste de não-vacuidade, porque teste de arquitetura verde é o que mais parece
funcionar quando não olha para nada.

## Funcionalidades

| | |
|---|---|
| **Chunking com contexto** | Fronteira em `##`, breadcrumb `H1 → H2` embutido, clamp de tamanho em tokens, com piso e teto a calibrar contra o corpus real |
| **Busca híbrida nativa** | Denso (1024-dim) + esparso, fundidos por RRF no próprio Qdrant |
| **Ingestão incremental** | Só reprocessa o que mudou, comparando uma fingerprint por ponto |
| **Poda da cauda** | A cauda de uma nota que encolheu é removida em toda ingestão, depois do upsert confirmado. Ponto de nota **apagada** é detectado e listado em toda rodada, e só apagado sob `--prune` |
| **Convergência sob falha** | Interromper a ingestão no meio nunca tira uma nota da busca — o pior caso é um ponto extra, que a rodada seguinte limpa |
| **Isolamento por integridade de código** | Nenhuma consulta sai do pacote de retrieval sem `tenant_id`, e uma suíte adversarial de dez casos verifica isso como gate no CI a cada push |
| **Conteúdo marcado como não confiável** | Resultados chegam ao agente delimitados como dado, não instrução, com os delimitadores escapados no texto recuperado |
| **CLI de operação** | Seis comandos: `schema`, `ingest`, `search`, `stats`, `eval` e `golden`. Reindex (`--full`), poda (`--prune`) e run filtrado (`--only`) são flags de `ingest` |
| **Medição de NFR sob comando** | `cmd/measure-ingest` e `cmd/measure-search` rodam contra o deploy real, dão **veredito** contra o teto do requisito, e gravam relatório durável em `out/` |
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
por consulta transformaria uma busca de algumas centenas de milissegundos em uma de onze segundos.

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
imagem, endereço de bind e credenciais obrigatórias lendo o arquivo, sem precisar de Docker nem da
máquina implantada, e o CI roda isso ao lado do linter.

#### As duas chaves do Qdrant, e por que são duas

O Qdrant implantado recebe **duas** credenciais, e nenhuma delas mora no repositório:

| Variável em `deploy/.env` | Vira, no compose | Quem apresenta |
|---|---|---|
| `QDRANT_API_KEY` | `QDRANT__SERVICE__API_KEY` | a CLI administrativa, como `KNOWRAG_ADMIN_QDRANT_API_KEY` |
| `QDRANT_READ_ONLY_API_KEY` | `QDRANT__SERVICE__READ_ONLY_API_KEY` | o servidor MCP, como `MCP_QDRANT_API_KEY` |

```bash
cp deploy/.env.example deploy/.env
# duas chaves geradas independentemente — nunca a mesma string nas duas linhas
openssl rand -base64 32   # QDRANT_API_KEY
openssl rand -base64 32   # QDRANT_READ_ONLY_API_KEY
```

**Não é sobre quem alcança a máquina.** Essa pergunta — o que uma máquina comprometida na rede
consegue tocar — foi decidida à parte, e uma chave read-only não mudaria a resposta dela. Esta é
outra: **o que o processo do MCP consegue fazer com a credencial que carrega**. Ele roda semanas sem
supervisão e responde com texto tirado de notas, que este código trata como entrada não confiável por
desenho — todo resultado sai envelopado como dado, não instrução. Um processo nessa posição segurando
uma chave que escreve tem raio de explosão estritamente maior que o mesmo processo segurando uma que
não escreve, com a rede inteira confiável ou não.

Quem enforça é o **Qdrant**, e é aí que está o valor. `internal/store` não expõe busca e
`cmd/mcp-server` nunca chama caminho de escrita, mas as duas são convenções que este repositório
mantém sozinho, por revisão. A chave read-only é a linha que sobrevive a alguém quebrá-las.

**As duas têm de ser valores diferentes, e é nisso que o arranjo inteiro se apoia.** Preencher as
duas com a mesma string não é uma versão mais fraca da separação — é a chave administrativa com um
segundo nome, e o Qdrant concede escrita a quem a apresenta. Nada no `docker-compose.yml` consegue
mostrar isso: para o Qdrant são duas chaves aceitas, e para quem lê o arquivo são duas referências de
variável que parecem certas. Quem confere é o bloco 5 do `scripts/verify-deploy.sh`, lendo
`deploy/.env` — e por isso **essa checagem não roda no CI**, onde o arquivo não existe. Ela roda na
máquina que tem as chaves, que é a máquina onde dá para errar. O script diz `NOT CHECKED` em vez de
ficar calado quando não encontra o arquivo.

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
knowrag ingest --vault both --dry-run  # conta chunks e lista órfãos, sem GPU e sem escrever
knowrag ingest --vault both
```

`--dry-run` não é offline, e não é sequer desconectado. O clamp conta tokens reais do BGE-M3 e se
recusa a aproximar, então ele exige `EMBEDDER_ENDPOINT` e faz um `POST /tokenize` por chunk; e ele
**lê o índice**, para poder listar as notas que seriam podadas. O que ele economiza é a GPU e a
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

### 5. Os dois drills

Dois procedimentos que se rodam de propósito, com um humano olhando. Ambos **destroem alguma coisa
de verdade**, e por isso exigem `--yes` *e* stdin num terminal — mais rígido que o `--prune` do
`ingest`, que aceita `--yes` **ou** um prompt respondido. Nenhum dos dois pergunta nada: um drill
pendurado esperando resposta é pior que qualquer das duas respostas.

```bash
cp scripts/drill.env.example scripts/drill.env   # preencher
git check-ignore -v scripts/drill.env            # confira que está ignorado, não presuma
set -a; . scripts/drill.env; set +a

./scripts/recovery-drill.sh --yes                        # derruba o índice inteiro e reconstrói
./scripts/prune-drill.sh --yes pessoal areas/nota.md     # apaga os pontos de uma nota e devolve
```

O que vai dentro de `scripts/drill.env` — nenhum desses valores pode nascer num arquivo rastreado,
que é por que o repositório guarda só o `.example`:

| Variável | Obrigatória | Para quê |
|---|---|---|
| `KNOWRAG_DRILL_SSH` | sim | como alcançar a máquina que roda o Qdrant, na forma que o `ssh` aceitar. Um `Host` do seu `~/.ssh/config` é a melhor forma aqui: aí o endereço nunca sai daquele arquivo |
| `KNOWRAG_DRILL_COMPOSE_DIR` | sim | o diretório **naquela máquina** de onde `docker compose down` e `up -d` são rodados. Não é o `deploy/` deste repositório |
| `KNOWRAG_DRILL_STATE_DIR` | não | onde as contagens de antes/depois e o transcript são escritos. Default `.drill`, que está no `.gitignore` |
| `KNOWRAG_DRILL_UP_TIMEOUT` | não | quantos segundos esperar o Qdrant responder depois da reconstrução antes de desistir e avisar que o índice está vazio. Default `300` |

`KNOWRAG_DRILL_COMPOSE_DIR` apontar para outro lugar que não o `deploy/` daqui não é descuido: os
dois arquivos divergiram no nome do volume, e é exatamente por isso que o drill descobre esse nome do
container em execução em vez de ler qualquer um dos dois.

**`recovery-drill.sh`** executa o que o runbook só descrevia: apaga o volume do Qdrant e reingere
tudo. Em cinco fases, com aborto entre elas — pré-voo (embedder, vaults, disco, Qdrant; reprovar
aqui **não destrói nada**), contagem antes gravada em arquivo, autorização, destruição e
reconstrução, contagem depois e veredito. Qualquer diferença nas contagens é reprovação.

O nome do volume sai do **container em execução**, nunca de um `docker-compose.yml`: o arquivo do
repositório e o da VPS divergiram nesse nome, e um script que chutasse criaria um volume vazio —
virando exatamente a perda que ele existe para evitar. É também por isso que ele usa
`docker compose down` seguido de `docker volume rm <nome descoberto>`, e não `down -v`.

**`prune-drill.sh`** tira uma nota do vault (move de lado, devolve por `trap` em qualquer saída),
reingere, confirma que ela é o **único** órfão do relatório, roda `--prune --yes`, e só aceita se os
totais caíram por exatamente os pontos daquela nota e exatamente um uid — é essa aritmética que prova
que nada de outro vault foi tocado. No fim devolve a nota e reingere até as contagens voltarem.

Os dois anunciam, em toda execução, o que **não** medem — indisponibilidade percebida, recuperação
parcial, falha no meio da reingestão — do mesmo jeito que `verify-deploy.sh`. Nenhum dos dois roda
em CI; o que roda lá são os testes que os dirigem contra fakes (`cmd/cli/drill_test.go`,
`cmd/cli/prune_drill_test.go`).

## Configuração

Tudo por variável de ambiente, ou por arquivo YAML apontado por `KNOWRAG_CONFIG_FILE` com as mesmas
chaves em `snake_case`. A obrigatoriedade é **por comando** — `schema apply` não exige as variáveis
do embedder, que ele nunca usa.

### CLI (`knowrag`)

| Variável | Para quê |
|---|---|
| `QDRANT_ENDPOINT` | `host:6334` — gRPC, o único protocolo que o código fala |
| `KNOWRAG_ADMIN_QDRANT_API_KEY` | chave **administrativa** do Qdrant — a CLI aceita qualquer `--tenant` e qualquer `--collection`, então a credencial dela é a do operador. O servidor MCP lê `MCP_QDRANT_API_KEY`, que é a [chave read-only](#as-duas-chaves-do-qdrant-e-por-que-são-duas), e nenhuma das duas cai para a outra |
| `EMBEDDER_ENDPOINT` | URL do serviço de embedding, ex.: `http://127.0.0.1:7999` |
| `DEFAULT_COLLECTION` | collection alvo |
| `LOG_LEVEL` | opcional, default `info` |
| `KNOWRAG_VAULTS` | os vaults desta instalação, separados por vírgula — ex.: `pessoal,trabalho` |
| `KNOWRAG_VAULT_<NOME>_PATH` | raiz do vault no disco |
| `KNOWRAG_VAULT_<NOME>_AREAS` | pastas de 1º nível que são `area` válida neste vault, separadas por vírgula |
| `KNOWRAG_VAULT_<NOME>_EXCLUDE_FOLDERS` | pastas ignoradas, separadas por vírgula — nome simples (1º nível) ou caminho relativo à raiz do vault, que ignora a subárvore inteira |
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

**Uma exclusão com barra ignora uma subárvore aninhada.** Uma entrada sem barra é nome de pasta de
1º nível e continua sendo só isso — uma pasta `rascunhos/` aninhada fundo dentro da área `beta/`
**não** é ignorada por uma entrada `rascunhos`. Uma entrada com barra é um caminho relativo à raiz do
vault, e ignora aquela pasta e tudo abaixo dela:

```bash
KNOWRAG_VAULT_PESSOAL_EXCLUDE_FOLDERS=templates,alfa/projetos/site-export
```

Isso ignora `templates/` de 1º nível e `alfa/projetos/site-export/`, e nada mais.

**O caso de uso é o `.md` que nunca foi nota.** Um briefing de sessão, um export, um `README.md` ao
lado de um `index.html` — arquivos cuja extensão é acidente — caindo numa subpasta dentro de uma área
onde moram notas de verdade. O sistema é fail-closed: um desses **aborta a ingestão inteira**, porque
não passa no contrato de frontmatter, e antes desta forma a única saída era excluir a **área toda**,
levando junto as notas boas. O erro hoje nomeia essa saída em vez de deixar o operador escolhendo
entre indexar lixo e perder uma área.

A comparação é de **caminho inteiro**, nunca de prefixo de texto: uma entrada `alfa/14` não casa uma
pasta `alfa/14-interno`. É insensível a maiúsculas, normaliza Unicode para NFC (um nome acentuado
vindo de macOS em NFD casaria com nada, silenciosamente), e `\` do Windows vale como `/`.

### Flags de `knowrag ingest`

| Flag | Default | Para quê |
|---|---|---|
| `--vault` | `both` | um nome de `KNOWRAG_VAULTS`, ou `both` para todos |
| `--dry-run` | desligado | avalia toda nota e relata o que uma rodada real faria, sem embedar e sem escrever. Lê o índice, então lista o que seria podado |
| `--full` | desligado | reindexa toda nota, pulando o atalho de integridade que normalmente deixa a inalterada em paz |
| `--only` | vazio | restringe a rodada às notas cujo `vault/path` casa um glob, ex.: `pessoal/areas/**` |
| `--prune` | desligado | **apaga** os pontos das notas que sumiram do vault. Destrutivo e opt-in: sem ele os órfãos são listados e deixados quietos |
| `--yes` | desligado | autoriza `--prune` sem perguntar. Obrigatório quando stdin não é terminal, onde não há ninguém para responder |
| `--grace-period` | `30s` | quanto tempo uma nota já sendo escrita tem para terminar depois do primeiro Ctrl-C; o segundo Ctrl-C a derruba na hora |
| `--json` | desligado | escreve o relatório da rodada como JSON no stdout e mais nada; o resumo do tokenizer vai para o stderr |
| `--tenant` | `interno` | o `tenant_id` sob o qual todo ponto é escrito |
| `--floor-tokens` | `256` | junta seções irmãs consecutivas abaixo desse tamanho |
| `--ceiling-tokens` | `1024` | quebra a seção acima desse tamanho |

**`--only` é recusado junto com `--prune`.** Uma rodada de subconjunto não consegue distinguir uma
nota apagada de uma que ela simplesmente não visitou, e a poda que confundisse as duas apagaria o
vault inteiro menos o glob.

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
knowrag eval --golden                      # recall contra o golden set
knowrag eval --golden --min-recall 0.80    # reprova (exit 4) abaixo do limiar
knowrag eval --isolation                   # a suíte de isolamento entre tenants
```

Exatamente um dos dois modos, obrigatório.

| Flag | Padrão | O que faz |
|---|---|---|
| `--file` | `docs/eval/golden-set.yaml` | Golden set a medir. Só com `--golden` |
| `--corpus` | *(vazio)* | Busca **neste arquivo** em vez da coleção do Qdrant. Só com `--golden` |
| `--min-recall` | `0` | Recall mínimo para passar. `0` registra o número sem reprovar |
| `--json` | — | Envelope JSON no stdout e nada mais |

O escopo não é flag: a coleção vem de `DEFAULT_COLLECTION` e o tenant é o mesmo padrão que a
ingestão usa, porque um gate que aceitasse `--tenant` mediria recall de um escopo que ninguém
ingeriu. O golden set ausente é erro nomeando o caminho (exit 2) — nunca "recall 0", nunca um passe.

**`--corpus` é o que torna o gate hermético possível.** Um corpus é um índice expresso como arquivo:
chunks com texto, pontuados por sobreposição de termos em vez de embeddings. Uma execução assim não
abre conexão, não precisa de embedder nem de GPU, e mede o *harness* de ponta a ponta — não a
qualidade de busca deste deploy. Número saído dali não entra em baseline nenhum.

**Os dois modos têm harness, e os dois rodam no CI a cada push.** O job `eval-golden-hermetic` roda
`knowrag eval --golden --corpus testdata/eval/hermetic/corpus.yaml`, checando duas coisas em passos
separados: o fixture (6/8 exatos) e o limiar; ver acima. O job `eval-isolation-hermetic` roda
`knowrag eval --isolation`, que constrói o próprio corpus e não abre conexão nenhuma. Os dois passam
por `scripts/ci/eval-gate.sh`, que existe para distinguir "reprovou" de "não tem harness" — hoje
nenhum dos dois modos consegue produzir a segunda resposta, e essa é a diferença entre este parágrafo
e o que ele dizia até ontem.

`--isolation` não aceita `--file`, `--corpus` nem `--min-recall`: a suíte não tem nota. Ou os dez
casos passam, ou ela reprova.

### `knowrag golden`

O gate de recall precisa de perguntas, e perguntas ninguém gera: alguém escreve. Este comando é a
sessão de autoria — ele sorteia uma nota, mostra o suficiente para você lembrar do que ela trata, e
pede **uma** pergunta que aquela nota deveria responder. Anexa o que você escreveu ao golden set e
sorteia a próxima.

```bash
export KNOWRAG_GOLDEN_AUTHOR="quem está escrevendo"   # cai para $USER se não existir
knowrag golden                                        # anexa a docs/eval/golden-set.yaml
knowrag golden --file caminho/para/outro-set.yaml
```

`--file` é a única flag. Não há `--json`: é uma conversa com uma pessoa, e envelope num stream que
ninguém parseia é flag que só é exercitada por teste.

**A sessão é incremental e retomável.** Ela lê o que já está no arquivo antes de começar, então uma
nota que já ganhou pergunta numa sessão anterior não é sorteada de novo. Sai com `q` ou uma linha
vazia depois de ter escrito alguma coisa; entra de volta amanhã de onde parou. `Enter` vazio pula a
nota atual sem gravar nada.

**Ele não sorteia uniformemente.** O sorteio vai para a **área mais carente** segundo a tabela
`coverage:` do próprio golden set — a mesma ordem que o relatório de progresso imprime, derivada uma
vez só, para "a área que o relatório diz estar mais curta" e "a área de onde vem o próximo cartão"
serem o mesmo fato e não dois que podem discordar. Sem uma tabela `coverage:` utilizável o comando
recusa antes de varrer os vaults, dizendo o que falta: a tabela é a única coisa que ele não pode
inventar, porque quantas perguntas cada área precisa é decisão de quem escreveu as notas.

Três recusas acontecem **antes** de qualquer varredura, que leva ~15 s: stdin que não é terminal (não
há ninguém para responder, e esperar penduraria a rodada), golden set inexistente, e autor
indeterminado. Um cron que invocasse isto por engano ouve "não há ninguém para responder" na hora, em
vez de depois de gastar a varredura.

**Ele nunca mostra resultado de busca, e essa é a razão de ele existir — não uma limitação.** Uma
pergunta escrita depois de ver o que o índice devolve é uma pergunta ajustada, inconscientemente, até
passar; um golden set feito assim mede a ferramenta que o produziu, não a recuperação. Por isso o
comando não lê o índice: ele lê os vaults, e não precisa de Qdrant nem de embedder para rodar.

**Essa invariante é sustentada por um teste de arquitetura, e a história de por quê vale mais que o
teste.** A autoria morava em `cmd/cli`, onde todo caminho até o Qdrant é uma declaração irmã no
`package main` e chamar uma não exige import nenhum. A regra era defendida por testes que liam o
fonte — varredura de substring, allow-list de símbolos, análise de taint, checagem de métodos dos
tipos que o comando segura. **Cinco revisões acharam cinco jeitos de passar por eles**, cada um de
uma linha, e os dois últimos não precisavam de nome nenhum sob vigilância: um pacote novo com nome de
função inocente, e um método declarado noutro arquivo do mesmo pacote. Cada correção era mais estreita
que o buraco que fechava, e a seguinte precisaria de inferência de tipos.

O conserto foi mover o código para `internal/goldenauthor`, onde a garantia passa a ser o **grafo de
imports**. `TestArch_GoldenAuthoringCannotReachTheIndex` (`internal/archtest/boundary_test.go`) deriva
o fecho transitivo do pacote a partir do fonte e pergunta se `internal/retrieval` ou `internal/store`
estão nele. Ele não nomeia símbolo, arquivo, nem pasta além desses dois. Três dos cinco desvios
deixaram de ser escrevíveis em vez de meramente ficarem vermelhos — o último deles exigiu separar
`internal/goldenset` de `internal/eval`, porque enquanto o schema do arquivo e o gate que busca eram
um pacote só, importar o primeiro trazia `eval.RunGolden` junto.

Dito com precisão, porque a versão ampla é tentadora e falsa: escrever o desvio **ainda compila**. Go
não tem como proibir um import a um pacote e permitir a outro. O que passou a custar é uma linha de
import de um pacote que este não tem outra razão para querer — e essa linha é exatamente o que o teste
percorre. Antes, não custava import nenhum e nada ficava vermelho.

**O que a sessão imprime a cada pergunta é deliberado, e é curto de propósito.** Três linhas: a regra
(*pergunte como perguntaria às 11 da noite, sem lembrar o nome do arquivo*), o critério (quanto mais
longe a sua redação estiver da redação da nota, mais isso mede recuperação em vez de casamento de
string) e **um** tipo de pergunta sugerido, rotacionado a cada turno. Uma tabela com os quatro tipos
seria lida como referência e pulada; sugerir um enviesa, e o viés é o mecanismo, não efeito colateral
— porque o viés contra o qual ele compete já existe: deixado à própria sorte, o autor escreve
perguntas de fato preciso em série, que é o tipo que vem à cabeça olhando para uma nota e o menos
informativo aqui, já que um fato preciso costuma dividir o termo raro com a nota e ser recuperado só
por ele.

Duas das seis sugestões do ciclo são "uma difícil", e isso não é acaso: um golden set de perguntas que
a busca já acerta **satura**. A 95% de recall não sobra espaço para piorar, nenhuma mudança futura
aparece como regressão, e o conjunto certifica que está tudo bem, para sempre. As perguntas que falham
hoje são as informativas — cada uma é um defeito de recuperação com endereço.

O `author` sai de `KNOWRAG_GOLDEN_AUTHOR` (ou `USER`) e a `date` de hoje. Os dois são documentação: o
que prova que uma pergunta foi escrita antes do resultado que ela avalia é o histórico git do golden
set, não um campo dentro dele.

### Servidor MCP (`knowrag-mcp`)

| Variável | Para quê |
|---|---|
| `MCP_QDRANT_ENDPOINT` | `host:6334` |
| `MCP_QDRANT_API_KEY` | a chave **read-only** do Qdrant — a de `QDRANT_READ_ONLY_API_KEY`, [nunca a administrativa](#as-duas-chaves-do-qdrant-e-por-que-são-duas). Nenhuma das duas cai para a outra |
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

## Medir os NFRs contra o deploy real

Dois binários à parte, em `cmd/measure-ingest` e `cmd/measure-search`. Nenhum dos dois roda em CI e
nenhum dos dois roda sozinho: é o operador que os aponta para o próprio deploy, pelas mesmas variáveis
de ambiente que a CLI já lê (`QDRANT_ENDPOINT`, `KNOWRAG_ADMIN_QDRANT_API_KEY`, `EMBEDDER_ENDPOINT`,
`DEFAULT_COLLECTION`, `KNOWRAG_VAULTS`).

**Os dois dão veredito, não número cru.** Cada um carrega o teto do requisito como literal, compara,
e imprime `passed` ou `FAILED` — porque um número solto na tela é uma coisa que alguém tem de lembrar
de comparar com alguma coisa, e ninguém lembra. Os dois também gravam **relatório durável em JSON**,
por default em `out/measure-<qual>-<timestamp>.json`, com o número, a decomposição, o teto e o
carimbo de tempo. `out/` está no `.gitignore`: os relatórios descrevem o vault e a infraestrutura de
quem rodou.

```bash
go build -o ~/bin/measure-ingest ./cmd/measure-ingest
go build -o ~/bin/measure-search ./cmd/measure-search

measure-ingest --tenant <TENANT>
measure-search --tenant <TENANT> --query "sua pergunta aqui" --query "outra pergunta" -n 50
```

### `measure-ingest` — NFR-5

Roda **uma ingestão incremental completa e real** contra o Qdrant e o embedder implantados — lock,
varredura, embed/escrita/detecção de órfão — e cronometra tudo ponta a ponta contra o teto de 60 s. O
que ele reporta além do total é a decomposição em três fases (`lock_acquire`, `vault_scan`,
`orchestrate`) mais o **não contabilizado**: total menos a soma das três. Essa sobra é impressa em vez
de dissolvida na última fase, para que um custo que este instrumento ainda não cronometra apareça como
lacuna visível.

| Flag | Default | Para quê |
|---|---|---|
| `--vault` | vazio (todos) | qual vault de `KNOWRAG_VAULTS` ingerir |
| `--tenant` | `interno` | o `tenant_id` sob o qual escrever e pelo qual travar o escopo |
| `--out` | `out/measure-ingest-<ts>.json` | onde gravar o relatório durável |

**Ele recusa dar veredito se a rodada não foi no-op.** O NFR-5 é sobre uma reingestão que **não muda
nada**; se alguma nota foi reprocessada, a rodada embedou e escreveu, e o relógio não responde a
pergunta que o requisito faz. Nesse caso ele imprime `NOT MEASURED — N de M nota(s) foram
reprocessadas` em vez de `passed` ou `FAILED`, e manda rodar de novo: a **segunda** rodada sobre um
vault inalterado é a que o requisito descreve. Isso existe porque o harness já reportou `FAILED` numa
rodada com 43 notas reprocessadas — o número estava certo e respondia a uma pergunta que ninguém
tinha feito. Rode uma vez para convergir o corpus, e meça na seguinte.

### `measure-search` — NFR-1b

Roda um lote de buscas híbridas reais, decompõe cada uma nas pernas de embedding da consulta, Qdrant
e overhead, e reporta p50/p95/p99 de cada coluna mais o total, contra o teto do NFR-1 (p95 ≤ 3 s,
p99 ≤ 5 s). Concorrência 1, sequencial, que é a condição de medição do próprio requisito — um
benchmark disparando em paralelo mediria um sistema diferente do que o teto descreve.

| Flag | Default | Para quê |
|---|---|---|
| `--query` | — | texto de consulta; **repetível**, e pelo menos uma é obrigatória |
| `--tenant` | — | **obrigatória**; o `tenant_id` de toda a busca |
| `-n` | `30` | quantas consultas rodar no total, ciclando pela lista de `--query` em ordem |
| `--collection` | `DEFAULT_COLLECTION` | a collection consultada |
| `--out` | `out/measure-search-<ts>.json` | onde gravar o relatório durável |

O veredito é sempre sobre o **total** medido independentemente, nunca sobre a soma de um subconjunto
das pernas: um overhead lento sozinho reprova, e há teste provando que uma versão construída de
`embed + qdrant` passaria no caso que essa reprova.

**Ele avisa quando a amostra é pequena demais para o percentil significar o que parece.** O rank é
nearest-rank, então o p99 cai na última amostra para todo `n` até 100: com `-n 30`, "p99" e "a pior
que a gente viu" são o mesmo número, e um outlier é a estatística inteira. O p95 cruza a mesma linha
em `n = 20`. O número continua valendo — é um teto real sobre o que foi observado — mas impresso ao
lado de p50 e p95 ele empresta a aparência de resolução dos vizinhos, e quem lesse um p99 passando
sobre 30 consultas estaria lendo **uma** consulta. Por isso o relatório imprime a frase junto do
veredito, em vez de deixar o leitor derivá-la. Os dois limiares saem da fórmula do rank, não de
convenção escolhida aqui.

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
rode o comando e ele descobre. Verificar um corpus inalterado custou **33,7 s** na instalação medida,
contra o teto de 60 s: o estado indexado do tenant é lido de uma vez, num snapshot só, e não por uma
consulta por nota. Esse número é o que o [`measure-ingest`](#measure-ingest--nfr-5) produz.

**O caminho normal é rodar quando fizer sentido para você** — depois de uma sessão de escrita, antes
de uma pesquisa importante, ou por um agendador seu (`cron`, `systemd timer`, Task Scheduler)
chamando o mesmo binário. O projeto não instala agendador nenhum, de propósito: quem sabe a cadência
certa é quem escreve as notas.

**Duas ingestões simultâneas não se atropelam mais.** Antes de ler o vault, `knowrag ingest` toma um
lock local do sistema operacional (`flock`) identificado por endpoint do Qdrant + collection +
tenant. A segunda execução no mesmo escopo é recusada na hora, com **código de saída 3** — separado
do `1` genérico justamente para um agendador distinguir "a anterior ainda está rodando" de "quebrou".
Nada é escrito e nada é lido do vault nessa recusa.

**`--dry-run` não toma lock, e hoje isso é escolha e não consequência.** Ele lê o índice — é assim
que lista o que seria podado — mas não escreve nada, então não tem nada próprio a proteger. O que uma
ingestão concorrente lhe custa é um snapshot que envelhece no meio do relatório, ou seja, um número
errado numa tela; a razão pela qual o caminho de escrita não pode aceitar isso, que é duas rodadas
apagando os pontos uma da outra, não se aplica a uma rodada que não emite delete nenhum.

O lock é liberado pelo kernel quando o processo morre, qualquer que seja a causa — matar uma
ingestão travada não deixa nada para limpar à mão.

> **Ele não atravessa máquinas.** É um lock local. Qualquer host com o binário, a credencial e rota
> até o Qdrant consegue rodar uma segunda ingestão sobre o mesmo escopo, e nada no sistema impede.
> A topologia prevista tem um executor só; isso é um acordo, não uma garantia técnica.

A ingestão parcial (`--only <glob>`) e a remoção de órfãos sob comando (`--prune`) existem, e as duas
são mutuamente exclusivas: ver a [tabela de flags](#flags-de-knowrag-ingest).

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
