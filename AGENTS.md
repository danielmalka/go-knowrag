# go-knowrag

## Este repositório é público

Trate tudo que é rastreado pelo git como publicado. Vale para código, `README.md`, mensagem de
commit, corpo de PR e nome de branch.

**Nunca** escreva em arquivo rastreado: nome de vault, caminho de disco de alguém
(`/home/<usuario>/...`, `/mnt/c/Users/...`), nome ou e-mail do dono, endereço ou IP de
infraestrutura, nome de host de rede privada. Quando um desses valores for necessário para rodar,
ele entra por variável de ambiente e o repositório guarda só um `.example` com placeholder — é assim
que `scripts/embedder-service/knowrag-embedder.env.example` existe.

## Decisão do dono fecha o item — e o documento é que está atrasado

Esta regra vem antes das outras porque foi a que mais gerou trabalho inútil. Quando o dono aceita um
risco ou dispensa uma verificação, **o item está fechado**. Não é "postergado", não é "pendência com
ressalva", não é "débito com gatilho". O documento que ainda o descreve como aberto está **atrasado
em relação à decisão**, e o conserto é editar o documento — não trazer o assunto de volta para ele
decidir outra vez.

A confusão tem uma forma específica e recorrente: o texto normativo foi escrito **antes** da decisão,
com a linguagem de quem ainda não sabia a resposta ("critério reprovando", "aguarda medição", "fora de
escopo nesta fase"). Quem lê depois encontra essa linguagem e a reporta como bloqueio. Em 2026-08-13
isso aconteceu com o critério 4 do ADR-003, com o limite de memória do Qdrant e com o D-35 — os três
já decididos, os três apresentados ao dono como perguntas em aberto. A resposta dele foi que já
estavam fechados, e ele estava certo.

Duas consequências práticas:

1. **Antes de listar algo como bloqueio, procure a decisão.** Se existir, o item não é bloqueio: é
   documento desatualizado. Corrija o texto na mesma passada e siga.
2. **Um DoD não tem autoridade sobre o dono.** O `PRD.md` chegou a exigir ADRs "em *Aceito* sem
   ressalva", redação que transformava aceitação consciente de risco em defeito. Critério que
   contradiz decisão tomada é o critério que está errado.

Reabre quando o **fato** que sustentou a decisão mudar — não quando alguém reler o texto antigo.

## O endurecimento de rede é decisão permanente — e credencial não é tudo a mesma pergunta

Foi tirado de escopo em 2026-08-11 e **aceito em definitivo em 2026-08-13**: não há ACL restrita por
destino, e a credencial que a ingestão usa é a administrativa mesma. Escolha informada, com a medição
na mesa (detalhes em `docs/`). Não proponha esse trabalho como parte de outra tarefa, e não o
registre como débito com gatilho na S12 — a S12 não é mais o gatilho. O que reabre é **uma segunda
máquina não confiável na rede**. Nada mais.

**Mas "separação de credenciais" nomeia duas coisas diferentes, e só uma delas foi dispensada.** A
distinção custou uma revisão para aparecer, e este parágrafo existe para ela não se perder de novo:

| | Pergunta | Estado |
|---|---|---|
| Raio de explosão desktop→VPS | *se a máquina que ingere for comprometida, o que ela alcança?* | **dispensado** — a rede é confiável por decisão |
| Escopo admin vs. read-only | *o que o processo pode fazer com a chave que carrega?* | **implementado** — o MCP tem chave read-only |

A segunda não é uma versão menor da primeira, e nenhuma decisão sobre a rede a responde. O servidor
MCP roda semanas sem supervisão respondendo com texto tirado de notas — que este código trata como
entrada não confiável por desenho, envelopando todo resultado. Uma chave que escreve nas mãos desse
processo é raio maior que uma que não escreve, com a rede inteira confiável.

Quem enforça é o Qdrant, e é aí que está o valor: `internal/store` não expõe busca e `cmd/mcp-server`
não chama caminho de escrita, mas as duas são convenções que este repositório mantém sozinho. A chave
read-only é a linha que sobrevive a alguém quebrá-las.

## O golden set real não bloqueia a entrega — decisão de 2026-08-13

Escrever as 60–90 perguntas de avaliação saiu de requisito e virou **"quem sabe um dia"**. O dono
decidiu assim sabendo o que perde, e o que se perde é específico: sem elas não há Recall@5 medido, e
a escolha entre busca híbrida e densa continua sendo a que o código já traz, sustentada por
raciocínio e não por número.

**O que isso não é.** Não é abandono do instrumento: o runner, o Recall@K com intervalo de Wilson, a
tabela de cobertura, a proveniência por git e o gate hermético existem, medem, e rodam no CI a cada
push. O que falta é conteúdo que só o dono pode produzir, porque a pergunta precisa ser dele e
precisa ser escrita antes de qualquer resultado ser visto.

Não trate a ausência do golden set como pendência que trava fechamento, e não a ofereça como próximo
passo obrigatório. Se ele quiser retomar, existe ferramenta para isso — a autoria assistida, que
sorteia uma nota e pede uma pergunta por vez.

Reabre por vontade do dono. Não reabre porque um documento de story ainda lista a tarefa.

## Embedder: processo de pé, modelo sob demanda, só GPU — decisão de 2026-11-15

Substitui a de 2026-08-16 ("não é 24 h", janela 07:00–23:00) e a do item 10 da lista de 2026-11-15
("sempre de pé na GPU, com fallback em CPU").

- **Só CUDA, sem fallback em CPU.** Sem GPU o serviço **não sobe** (sai com status 78, mensagem clara) e o
  `/handshake` falha com 503. "Sem CPU" é sobre o dispositivo, não a precisão: bf16 (padrão) e fp32 rodam na
  GPU; **fp16 é recusado** pelo próprio serviço.
- **O processo fica de pé; o modelo não.** `knowrag-embedder.service` não carrega nada ao subir. O modelo carrega
  na primeira chamada que precisa dele e é **descarregado** (referência solta, `gc`, `empty_cache`) depois de
  `KNOWRAG_EMBEDDER_IDLE_UNLOAD` segundos sem chamada (padrão 600; 0 = nunca). A primeira busca depois de uma
  pausa paga a carga, então o timeout do cliente MCP tem de ficar acima dela.
- **Ingestão em horário fixo** (`knowrag-ingest.timer`), sem janela de ligar/desligar o embedder e sem
  `embedder-up`/`embedder-down`. O serviço roda uma chamada de cada vez (a busca passa à frente dos lotes).
- **Só loopback, por desenho.** O serviço não tem autenticação; por isso `--host`/`KNOWRAG_EMBEDDER_HOST` só
  aceita `127.0.0.1`, `::1` ou `localhost` (este vira o literal `127.0.0.1` antes do bind, para o sistema nunca
  resolver um nome), e qualquer outro valor encerra o processo com status 78. Não há
  opt-in para rede: expor o serviço exigiria antes uma autenticação, como decisão própria.
- **Dependências Python com versões exatas** em `scripts/embedder-service/requirements.txt`.
- Detalhes, números medidos e operação: `scripts/embedder-service/README.md`.

## `docs/` fica fora do git, e isso é decisão fechada

`docs/` está no `.gitignore` de propósito: é onde moram os ADRs, o PRD e o registro de débitos
técnicos, e esse material cita vault, infraestrutura e informação pessoal do dono.

Não proponha versionar `docs/`, nem espelhar, nem publicar "sumários anonimizados", nem copiar
trecho de lá para arquivo rastreado. O custo disso — a base de decisão do projeto existir em uma
cópia só — foi medido, discutido e **aceito** em 2026-08-10, e está registrado no próprio
`docs/debitos-tecnicos.md` como D-14. Reabrir o assunto é contrariar uma decisão tomada, não pagar
um débito.

O `.gitignore` protege essa pasta. Ele não protege nada que nasça fora dela — por isso a regra
acima, sobre o que é público, é a que exige atenção na hora de escrever.

## As pastas excluídas da ingestão são decisão fechada

O dono decidiu em 2026-08-11, com a medição na mesa, manter fora do índice as pastas que a
configuração já exclui. **A validade do frontmatter nunca foi o motivo** — as pastas em questão
estavam medidas e prontas para entrar, e a escolha foi de conteúdo: material operacional não deve
competir, na busca, com as notas que o dono escreveu.

Não proponha incluí-las alegando que "agora o frontmatter está válido". Uma delas, além disso, guarda
templates cujos campos são placeholders por definição: eles são inválidos *porque* estão corretos, e
"consertá-los" destruiria a função. Detalhes e números em `docs/debitos-tecnicos.md`, D-10.

Reabre se o dono mudar de ideia sobre o conteúdo. Não reabre por remedição.

## O registro de débitos envelhece, e isso já custou trabalho

`docs/debitos-tecnicos.md` descreve o mundo de quando cada item foi escrito. Pagar uma dívida no
código não atualiza o texto, e ninguém relê o arquivo inteiro. Em 2026-08-11, de seis itens
atacados, **quatro descreviam uma realidade que já não existia** — um nunca tinha sido verdade, um
já estava pago havia dois dias, e dois estavam parcialmente desatualizados.

Antes de trabalhar num item deste arquivo, **confirme contra o código de hoje que o problema ainda
existe**. Vale principalmente para a frase que define a gravidade: ela costuma soar como conclusão do
parágrafo anterior e ser, na verdade, uma afirmação própria que ninguém verificou.

## Quando um comentário afirma um número, o número mora neste arquivo?

Esta é a pergunta que mais achou defeito neste repositório, e ela é específica de propósito.
"Revise os comentários" ninguém segue. Esta dá para responder em dez segundos, olhando uma linha.

Se o número está aqui — `const`, literal, campo de struct — o comentário é verificável na hora. Se
ele é **decidido em outro arquivo**, o comentário está afirmando o que outro arquivo faz, e é aí que
mora o defeito: o outro arquivo muda e ninguém volta aqui. Em 2026-08-11 isso apareceu **cinco vezes
numa tarde**, e nenhuma era bug de lógica — todas compilavam, passavam em lint e em `-race`, e três
passavam nos próprios testes:

- um teto de 10 s que o `Handshake` sobrescrevia por dentro com 4 s, deixando a verificação de boot
  desligada no caso comum;
- um teste de orçamento que somava uma das duas pernas e ficava verde enquanto o total real
  ultrapassava o deadline;
- um teste que conferia o número escolhido sem conferir se ele é usado;
- um teto justificado por "o serviço responde devagar enquanto carrega o modelo" — o serviço faz
  bind **depois** de carregar, então não responde devagar, recusa na hora (`server.py`);
- e, no texto escrito para consertar o anterior, "ou pego no meio do desligamento" — não existe
  meio-desligamento, o `server.py` não instala handler de SIGTERM.

Duas consequências práticas:

1. **Comentário que descreve mecanismo de outro pacote envelhece em silêncio.** Escreva o nome do
   arquivo junto da afirmação, para que a próxima pessoa saiba onde conferir.
2. **Um teste de orçamento que esquece uma perna certifica o número que esqueceu de somar.** Se a
   asserção envolve tempo, some todas as pernas do caminho real e prove com um plante que ela
   reprova quando alguma cresce.

E a leitura que fecha o ciclo: numa rodada de plante de defeito, **a lista que importa é a dos testes
que não ficaram vermelhos**. É nela que o teste vazio se esconde, e é a que ninguém lê.

### A variante que nenhum plante pega: dados de teste que fazem duas fórmulas coincidirem

Em 2026-08-13, uma função de percentil documentava `ceil(p*n)` e implementava truncamento. Todos os
testes usavam **n=100**, onde `p*n` cai redondo para p50, p95 e p99 — e aí `ceil` e `floor` dão o
mesmo índice. A suíte inteira ficava verde provando uma fórmula que o código não implementava.

O custo apareceu no tamanho que a ferramenta recomenda: com **n=30**, `int(0.99*30)` é 29, então o p99
lia a segunda pior amostra. Uma consulta de 9 s entre trinta de 10 ms saía como `p99 = 10ms`, e o
veredito dizia que passou. A ferramenta existia para achar aquela consulta.

**Plantar não ajuda aqui**, e é isso que torna o caso distinto do resto desta seção: qualquer plante
sobre a fórmula errada continua verde com aqueles dados. O que acha é a pergunta:

> **Existe entrada em que a fórmula documentada e a implementada dariam respostas diferentes — e ela
> está nos testes?**

O vizinho passa nesse critério e mostra como é quando dá certo: `WilsonInterval`
(`internal/eval/confidence.go`) é testado com **0 de 50** e **50 de 50**, casos em que a aproximação
normal ingênua daria intervalo degenerado `(0,0)` e `(1,1)` e o Wilson não. Os dados **separam** as
fórmulas. Se os casos fossem todos de proporção intermediária, o teste passaria com qualquer uma das
duas.

## Três modos de falhar em silêncio, todos achados numa fase de plantes

Em 2026-08-11, fechando a S06b, a rodada de plantes achou três coisas que **não** são bug de lógica e
que nenhuma delas fica vermelha sozinha. As três se parecem: alguma coisa que deveria provar algo
para de provar, sem avisar, e continua na lista como se provasse.

1. **Plante que deixou de aplicar não prova nada.** Corrigir uma linha invalida em silêncio todo
   plante que a citava — e a regularidade é perversa: **o plante morre exatamente onde o código mais
   mexeu.** Aconteceu quatro rodadas seguidas. Um plante inerte é indistinguível de um plante que
   passou e some no meio de quarenta linhas verdes. Filtre a saída por "não aplicou" e "nada ficou
   vermelho", e leia **só** essa saída. Plante que não compila entra na mesma categoria: prefira
   **negar a condição** a apagar código, porque apagar costuma deixar import ou variável sem uso.

2. **Teste que certifica o texto errado.** Um teste sobre prosa — mensagem de help, texto de
   relatório — envelhece junto com o comportamento e trava a versão velha no lugar. A S06b tinha um
   teste exigindo que o help do `--dry-run` dissesse *"nunca lê o índice"* depois de a mesma PR fazer
   o dry run ler o índice: corrigir o help reprovava o teste. O que existia para proteger o operador
   virou a tranca da mentira, e passa verde enquanto faz isso. Se um teste afirma texto, ele precisa
   também **recusar** a redação antiga — a lista de ausentes é o que impede a regressão de voltar.

3. **Comentário que corrige outro não cita o texto errado, descreve o mecanismo.** Frase falsa
   preservada entre aspas é indistinguível de frase falsa esquecida — para um grep, e para quem lê
   rápido. Uma correção assim chegou a fazer a verificação óbvia responder "não corrigido" sobre
   código corrigido.

## Plante contra artefato que não é Go precisa de `-count=1`, e isso mente exatamente como um teste vazio

Em 2026-08-13 isto aconteceu **duas vezes no mesmo dia, por pessoas diferentes, sem uma saber da
outra** — e as duas quase reportaram testes bons como vazios.

Quando o alvo do plante é um **script**, um YAML, uma fixture — qualquer coisa que o processo do
`go test` não abre —, o cache de teste **replica o verde por cima do plante**. O Go só invalida cache
por arquivo Go e por variável de ambiente; um `verify-deploy.sh` alterado não é entrada que ele
rastreia. A saída é indistinguível de "o plante aplicou e nada reprovou", que é justamente a lista que
este arquivo manda ler com atenção.

Um dos dois casos foi pior e vale como advertência específica: o plante trocava a string `tail -1`, e o
**comentário logo acima do código contém esse literal**, escrito para explicar a escolha. O plante
aplicou, o arquivo mudou, nada ficou vermelho — porque ele editou a explicação, não a regra. Comentário
bom, que cita o código que explica, **aumenta** a chance de o plante errar o alvo.

Duas exigências, e as duas são baratas:

1. **`-count=1` em toda rodada de plante** que não seja puramente Go.
2. **O harness afirma que aplicou antes de rodar** — comparar o arquivo com o original, ou casar a
   linha exata e falhar alto se ela não existir. Plante que não aplica precisa gritar, não sumir no
   meio de quarenta linhas verdes.

## Quando um plante não fica vermelho, o conserto costuma ser tirar o lugar onde dá para errar

Cinco vezes na mesma fase, um plante que não reprovou nada apontou para a mesma coisa: a guarda
estava num *call site*, e o call site é esquecível. Nos cinco casos o conserto **não** foi escrever
um teste para o call site — foi mover a condição para junto de quem executa a ação, ou apagar a
possibilidade:

- as duas autorizações da poda foram para dentro da função que deleta;
- a verificação de disco também, depois que o plante que a removia do chamador não reprovou nada;
- a flag de "run filtrado" passou a ser derivada das opções em vez de recebida como booleano;
- a interrupção passou a ser checada dentro da poda, não antes de chamá-la;
- e o conjunto de caminhos vivos passou a sair da **mesma função** que produz o conjunto de uids
  vivos — dois helpers irmãos ainda deixariam um chamador invocá-los sobre listas diferentes.

O último é o caso limite e vale a regra: **quando o defeito deixa de ser representável, o plante que
o provaria não pode ser escrito, e isso é o resultado — não uma lacuna.** Reintroduzir a
possibilidade para ter o que plantar é construir o defeito para poder testá-lo.

<!-- BEGIN CONTEXT-KIT -->
# Context Kit

This repository is linked to Context Kit: the `.context-kit/` directory at its root carries the
Contracts and the project config.

**Before writing any production code, read the kit's `core/00-entrypoint.md`.** Its location is
machine-specific and deliberately not recorded here: `ck install` writes it into your own global
agent config, and `ck` reads it from `$CK_HOME`, which defaults to `~/.context-kit`.

Rule zero, restated here because it is the one that matters: no production code is written
before a signed Contract exists for the work.

Do not have the kit installed? Install it, then run `ck link .` — see this repository's README.

Kit version: 2.1.0
<!-- END CONTEXT-KIT -->

## O projeto está desligado desde 2026-08-21 — e isso é decisão, não avaria

O dono parou de usar a busca e mandou desligar tudo. Não trate nada disto como quebrado:

- os timers de usuário instalados nesta máquina (`knowrag-ingest`, `knowrag-ingest-day`, `knowrag-embedder-up`,
  `knowrag-embedder-down`) estão **stopped e disabled** (o repositório hoje só tem `knowrag-ingest.timer`; os
  outros três são cópias antigas instaladas, a remover ao religar);
- o `knowrag-embedder.service` está parado e o venv com os pesos do BGE-M3 foi **apagado do disco**;
- o servidor MCP saiu do config do cliente de agente, então as duas ferramentas não existem mais;
- o Qdrant da VPS foi derrubado com `docker compose down` **sem `-v`** — o volume
  `knowrag_qdrant_storage` continua lá, com as 3.901 entradas da collection `interno` intactas.

Religar é reinstalar o venv, baixar o modelo de novo, subir o compose e reabilitar os timers. O
índice não precisa ser reconstruído enquanto o volume existir — e, se for apagado, a reingestão o
reconstrói, porque ele é derivado do vault.

Não proponha religar, não "conserte" os timers desabilitados, e não leia a ausência do embedder
como bug de instalação. Reabre por vontade do dono, e só.

## Decisões do dono de 2026-11-15 — segurança do dossiê, CI e embedding

Registradas aqui porque nenhuma delas é visível no código. Esta seção é só destas decisões; as de
escolha de embedding ficam em seção própria.

- **Riscos de segurança do dossiê aceitos como estão.** O gRPC do app com o Qdrant sem TLS, a porta
  6334 do compose da raiz publicada em todas as interfaces e o embedder HTTP sem autenticação
  continuam como estão: o acesso a esses serviços é só pela rede privada (tailnet). É **risco
  aceito**, não pendência. A 6334 **não será restringida**. O que reabre é uma segunda máquina não
  confiável na rede — a mesma condição da decisão de rede acima.
- **A `main` passa a exigir o CI (`test` + `lint`)**, por proteção de branch do GitHub, em vez de
  depender só de disciplina local. `lint` vermelho na `main` é bloqueio, não ruído.
- **As dependências Python do embedder são fixadas** (versões exatas) **junto com a troca de embedding**
  (cartão 173): `scripts/embedder-service/requirements.txt`.
- **O volume do Qdrant na VPS é mantido** até essa mesma decisão. Não o apague nem o recrie antes dela.
- **O dossiê de código fica fora do git**, em `docs/dossier/` local, como o resto de `docs/` (D-14, ver
  acima). Não o versione nem copie trecho dele para arquivo rastreado.

## Escolha do modelo de embedding — decisões de 2026-11-15

Decisões do dono, que valem para qualquer trabalho de troca de modelo:

1. **Só modelo aberto e local.** Nada de API de embedding: o vault é pessoal e não sai da máquina.
2. **Dois candidatos foram provados:** `google/embeddinggemma-2` (só texto; bf16 ou fp32, **nunca fp16**, que
   dá NaN em silêncio; prefixos do model card: `task: search result | query: ` na pergunta e
   `title: … | text: ` no documento) e `Qwen/Qwen3-Embedding-0.6B` (com a instrução de query do card). Os dois densos.
3. **Sem BM25 nem nenhum vetor esparso ou lexical.** O sistema futuro é só denso. A referência do bge-m3 é a
   medição de 2026-08-16 (híbrido 25/30, denso 21/30); o bge-m3 não foi baixado nem rodado na prova.
4. **O Qwen3-Embedding-0.6B sai da disputa.** Ficou atrás do EmbeddingGemma 2 em todos os protocolos, pesa mais
   que o dobro e em CPU é três vezes mais lento.
5. **O EmbeddingGemma 2 é o candidato.** A troca **não** está decidida: ela espera a medição com o golden set
   ampliado (item seguinte).
6. **O golden set vai a ~60 perguntas, sem as inalcançáveis.** As 6 perguntas cuja nota esperada hoje é
   `private` ou `archived` e a que aponta para uma nota que já não existe saem; novas perguntas, escritas a
   partir do vault atual por quem não sabe qual modelo acertou o quê, entram no lugar. Só então se decide.
7. **A rodada com o golden set ampliado roda só o EmbeddingGemma 2** (denso, bf16, com os prefixos do card),
   **sem rodar o bge-m3.** O dono compara o resultado do EmbeddingGemma 2 e decide a troca; o bge-m3 não é
   remedido nesta rodada.
8. **A prova de 30 perguntas e a rodada ampliada rodam na mesma cópia de referência do vault**, com as mesmas
   exclusões, para que os números sejam comparáveis. Migrar a medição para o vault atual do projeto depende
   de uma revisão do dono.
9. **Critério de virada:** Recall@5 de produção ≥ 0,70 no golden v2 e nenhuma área com zero acertos, medido na
   cópia de referência do vault. A troca para o vault atual vem depois da revisão do dono.
10. **O embedder roda só na GPU, sem fallback em CPU; o processo fica de pé e o modelo carrega sob demanda e
    descarrega por ociosidade** (seção "Embedder: processo de pé…" acima). Isso fecha o D-41 (a janela diária do
    embedder).
11. **Uma ADR-008, local, substitui a ADR-001**; o resumo público fica no README e neste arquivo.
12. **Sem CHANGELOG.**
13. **As violações de contrato do vault são corrigidas antes de reingerir** (cartão 184); a produção recusa um
    vault com violações.
14. **Não há rollback para o bge-m3.** Se a migração não atingir o critério do item 9, o serviço continua desligado.

**Contrato de ingestão com o perfil do EmbeddingGemma 2.** O perfil (`internal/schema/model_profile.go`)
fixa `MaxInputTokens` 8192 e `PromptReserveTokens` 128; o chunker usa a janela `WindowTokens()` = 8064. O
título da nota vai com cada chunk (`title: <título> | text: `) e **é cortado** em `TitleMaxTokens()` = 96
(reserva − 32 de moldura) por `fitTitle` (`internal/ingest/title.go`), só na cópia enviada ao embedder:
nunca se corta o texto do chunk, e o título do payload e do `point_hash` fica inteiro. Invariante, testada
para todo perfil do registro: janela + título + moldura ≤ `MaxInputTokens`. Ao mudar a reserva, o prefixo
ou o tokenizador, revise `promptFramingTokens`; `Deps.TitleMaxTokens` é obrigatório quando o handshake
traz `PromptVersion`.

**Resultado da prova** (30 perguntas, Recall@5, mesma conta do `eval --golden`: acerto = a nota esperada está
no top 5; consulta densa de produção reproduzida; os dois modelos em bf16 na GPU, sobre os mesmos 4.661 chunks):

| Modelo (denso) | produção | agosto | relaxado | pesos / VRAM / RSS | ingestão | query p95 GPU / CPU |
|---|---:|---:|---:|---|---:|---:|
| EmbeddingGemma 2 (768d) | 21/30 | 22/30 | 25/30 | 0,5 / 0,6 / 1,9 GB | 152 s | 78 ms / 118 ms |
| Qwen3-Embedding-0.6B (1024d) | 19/30 | 21/30 | 24/30 | 1,1 / 1,3 / 2,2 GB | 214 s | 60 ms / 378 ms |

**O corpus mudou desde 2026-08-16**, então não é comparação exata com 21/30 e 25/30: são 830 notas e 4.661
chunks (em agosto, ~735 notas e ~3.690 pontos), 1 nota do golden set não existe mais e 6 perguntas apontam
para notas hoje `private` ou `archived` (duas eram `stable` em agosto). Por isso três protocolos de
visibilidade: **produção** (arquivadas e privadas excluídas, como o `eval --golden`; teto 23/30), **agosto**
(arquivadas incluídas, privadas excluídas; teto 25/30) e **relaxado** (nada excluído; teto 29/30).

Leitura: o EmbeddingGemma 2 fica 1 a 2 acertos à frente do Qwen3. Contra o bge-m3 ele empata o denso (21) em
produção e empata o híbrido (25) só no relaxado, que ignora a visibilidade; em produção fica 4 abaixo do
híbrido. A memória cai bem na VRAM (0,6 GB contra ~1,2 GB do bge-m3) e menos no RSS (~1,9 GB contra ~2,4 GB;
a maior parte é torch). Em CPU responde a query em ~118 ms no p95 com ~2,4 GB de RSS, o que tornaria possível
um embedder sempre de pé sem GPU (D-41). Diferença de 1 a 3 acertos com 30 perguntas não decide, e é por isso
que o golden set cresce antes da troca. A ferramenta da prova (exportador de chunks e script de avaliação) fica
fora da `main`.

**Rodada ampliada** (golden v2 com 60 perguntas, só o EmbeddingGemma 2 denso, bf16 na GPU, mesma cópia de referência
do vault e mesmos 4.661 chunks da prova): **46/60 de Recall@5 (0,767; IC95 0,646–0,856)**, e o resultado é o mesmo nos
três protocolos de visibilidade porque o golden v2 só aponta para notas visíveis à busca. Nenhuma das 7 áreas do
golden ficou com zero acertos (o pior resultado foi 1 de 3). A checagem exata por força bruta também deu 46.
Ingestão dos 4.661 chunks em 144 s; query com p95 de 73 ms na GPU (outra execução, com a GPU compartilhada, deu
153 ms) e de 121 ms em CPU fp32 (RSS 2,4 GB). **Pico da ingestão:** 5,2 GB de ativação com batch 16 e chunks de até
~3.900 tokens, contra 0,5 GB dos pesos; para o embedder ficar de pé junto com outros processos a ingestão precisa de
batch menor ou de teto menor de tokens, e isso se mede antes de fixar o serviço.
