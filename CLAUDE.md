# go-knowrag

## Este repositório é público

Trate tudo que é rastreado pelo git como publicado. Vale para código, `README.md`, mensagem de
commit, corpo de PR e nome de branch.

**Nunca** escreva em arquivo rastreado: nome de vault, caminho de disco de alguém
(`/home/<usuario>/...`, `/mnt/c/Users/...`), nome ou e-mail do dono, endereço ou IP de
infraestrutura, nome de host de rede privada. Quando um desses valores for necessário para rodar,
ele entra por variável de ambiente e o repositório guarda só um `.example` com placeholder — é assim
que `scripts/embedder-service/knowrag-embedder.env.example` existe.

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
