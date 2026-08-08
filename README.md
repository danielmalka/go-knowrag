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

> **Status: especificação completa, implementação não iniciada.** Tudo abaixo descreve o sistema
> como está especificado — PRD, stories e ADRs —, não código rodando.

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


## Licença

MIT — ver [`LICENSE.md`](LICENSE.md).
