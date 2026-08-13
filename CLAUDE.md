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
