# agentmesh

Motor local, standalone, para rodar vários agentes de terminal (`claude`,
`codex`, `gemini`, `opencode`, ou shells comuns) lado a lado e fazê-los se
comunicar: mandar mensagem, delegar tarefa e esperar o resultado, rodar
comando num shell e ler a saída de volta.

Extraído do motor de orquestração do [Openfield](https://github.com/NatanBack77/Openfield)
(o app de canvas para agentes), removendo tudo que é específico de GUI
(Wails, canvas, kanban, notas, workspaces) — sobrou só o núcleo de
comunicação entre agentes, para rodar puro no terminal.

## Como funciona

Um único processo (`agentmesh serve`) spawna cada agente **dentro da sua
própria sessão [tmux](https://github.com/tmux/tmux)** — é o tmux quem faz a
emulação de terminal de verdade (tamanho, scroll, redraw), não uma PTY
gerenciada à mão. O motor só lê a tela renderizada (`tmux capture-pane`)
pra detectar quando o agente termina o turno (regex sobre o texto — funciona
com qualquer CLI, sem precisar que o agente coopere) e expõe uma API HTTP
local (loopback, `127.0.0.1`) com as primitivas de comunicação. Todo agente
registrado é peer de todos os outros — não existe passo de "desenhar uma
seta" como no Openfield: é uma malha (mesh) plana.

**Requer `tmux` instalado** (`sudo apt install tmux` / `dnf` / `brew`) —
único pré-requisito de sistema além do binário do `agentmesh`.

## Instalar numa máquina nova

Pré-requisitos: [Go](https://go.dev/dl/) 1.21+ (`go version` pra conferir)
e `tmux` (`tmux -V`). Este repositório é privado — quem for instalar
precisa ter acesso a ele (te peça pra adicionar como colaborador, ou use um
token/chave SSH sua já autorizada).

```bash
git clone git@github.com:NatanBack77/agentmesh.git
cd agentmesh
./scripts/install.sh
```

O script compila e coloca o binário em `~/.local/bin/agentmesh`. Se
`~/.local/bin` não estiver no `PATH`, ele avisa e mostra a linha pra
adicionar no `~/.bashrc`/`~/.zshrc`. Sem acesso à internet pra clonar mas
com o binário já compilado em outra máquina Linux/amd64 equivalente, basta
copiar o arquivo `~/.local/bin/agentmesh` — é um binário estático, sem
instalador (só precisa do `tmux` já instalado no destino).

Depois de instalado, confirme:

```bash
agentmesh --help
```

### Deixar o motor sempre rodando (opcional)

Qualquer comando (`agentmesh spawn`, `agentmesh ls`, ...) já sobe o motor
sozinho em background se não encontrar um rodando — não precisa fazer nada
manual. Se preferir controlar isso à parte (por exemplo, pra ele sobreviver
mesmo sem nenhum terminal aberto), existe uma unidade systemd de usuário
pronta em `scripts/agentmesh.service`:

```bash
mkdir -p ~/.config/systemd/user
cp scripts/agentmesh.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now agentmesh

# opcional: sobreviver a reboot mesmo sem login gráfico/SSH ativo
loginctl enable-linger "$USER"

# conferir
systemctl --user status agentmesh
journalctl --user -u agentmesh -f
```

## Teste automático (um comando só)

```bash
agentmesh demo
```

Sobe o motor sozinho se ele não estiver rodando, spawna duas sessões
`claude` de verdade em diretórios isolados, passa pelas telas de boot
(tema/confiança) sem você apertar nada, espera as duas ficarem prontas,
manda uma instruir a outra a chamar `agentmesh send` sozinha (via a
ferramenta Bash dela — comunicação real entre agentes, não simulada), e
confirma que a mensagem chegou na tela da segunda. No fim deixa as duas
rodando pra você explorar (`agentmesh attach alpha`) ou `agentmesh kill`
pra encerrar. Use `--agent codex` (ou outro provider) pra testar com outra
CLI no lugar de `claude`.

Nota: a detecção de "o agente terminou o turno" é feita por regex sobre a
tela (não existe API oficial pra isso em nenhum desses CLIs) — às vezes ela
acerta cedo ou tarde demais, e telas de diálogo (tema, confiança) podem se
parecer com um prompt pronto. O motor já reconhece e ignora os diálogos
conhecidos do Claude Code; se ainda assim travar num CLI diferente, rode
`agentmesh attach NOME` pra ver a tela com os próprios olhos e destravar na
mão (é um `tmux attach` de verdade — funciona igual a entrar em qualquer
sessão tmux).

## Uso

```bash
# 1. spawna os agentes (o motor sobe sozinho se não estiver rodando)
cd ~/meu-projeto                # sem --cwd, usa o diretório atual
agentmesh spawn coder    claude
agentmesh spawn reviewer claude

# 2. lista quem está rodando
agentmesh ls

# 3. entra interativamente num agente — é um tmux attach de verdade
agentmesh attach coder          # Ctrl+B D desanexa sem matar o processo

# 4. de outro terminal, ou de dentro de um dos agentes via Bash tool:
agentmesh send reviewer "dá uma olhada no PR aberto"        # não bloqueia
agentmesh handoff coder "implementa X" --timeout 300        # bloqueia e traz o resultado
agentmesh exec meushell "npm test"                           # só em agentes shell

agentmesh whoami                # identidade do agente atual
agentmesh watch reviewer        # acompanha, somente leitura (tmux attach -r)
agentmesh kill reviewer
```

Dois agentes podem apontar pro **mesmo** diretório sem problema (é só
repetir o `--cwd`, ou rodar os dois `spawn` da mesma pasta).

Quando o agente spawnado é `claude`, o `agentmesh` já injeta via
`--append-system-prompt` a instrução de que ele pode usar a CLI `agentmesh`
(disponível no PATH, com `AGENTMESH_TERMINAL_ID`/`AGENTMESH_URL` já no
ambiente) através da ferramenta Bash para falar com os outros agentes —
então um `claude` pode chamar `agentmesh peers` / `agentmesh send` /
`agentmesh handoff` sozinho, sem intervenção sua. Para qualquer provider
(inclusive shells puros), o motor também escreve `.agentmesh/peers.md` no
diretório de trabalho de cada agente — uma lista simples de quem mais está
rodando, legível por qualquer ferramenta de leitura de arquivo.

## Qualquer modelo de IA, não só os quatro citados acima

`claude`/`codex`/`gemini`/`opencode` só ganham detecção de turno afinada
(regex específico pro jeito que cada um desenha o prompt). Qualquer OUTRO
comando funciona igual, sem lista de permissão nenhuma — `agentmesh spawn
nome qualquer-cli-no-path` cai num padrão genérico de prompt (`$`/`>`/`❯`).
Se você tiver um CLI de IA diferente instalado, já dá pra usar hoje.

## Primitivas

- **`send`** — entrega a mensagem agora (se o agente estiver ocioso) ou
  enfileira (se estiver processando); não bloqueia quem chamou.
- **`broadcast`** — igual ao `send`, mas manda pra TODOS os outros agentes
  registrados de uma vez.
- **`handoff`** — entrega a mensagem, espera o agente terminar o turno, e
  devolve o que ele produziu. Bloqueante, com timeout configurável e
  detecção de deadlock/ciclo na cadeia de delegação.
- **`exec`** — igual a `handoff`, mas só aceita alvos que sejam shells
  puros (`bash`/`zsh`/`sh`/`fish`) — é a forma seguro de um agente rodar um
  comando "no terminal visível do usuário" sem escrever na entrada de
  outro agente.

## `--role`: dar uma persona ao agente já no spawn

```bash
agentmesh spawn revisor claude --role "Você revisa código em busca de bugs de segurança. Nunca escreva código novo, só aponte problemas."
```

O texto é entregue como a primeira mensagem do agente, assim que ele sai
das telas de boot — funciona pra qualquer provider (não só claude).

## Isolamento por git worktree

Duas (ou mais) sessões trabalhando no MESMO repositório ao mesmo tempo,
sem pisar no trabalho não commitado uma da outra:

```bash
agentmesh spawn coder claude --cwd ~/meu-repo --worktree
agentmesh spawn fixer claude --cwd ~/meu-repo --branch bugfix/login
```

Cada agente ganha seu próprio `git worktree` (checkout físico separado,
mesmo `.git` compartilhado — barato, não é um clone) numa branch própria.
Sem `--branch`, o nome vira `agentmesh/<nome-do-agente>`; com `--branch`,
`--worktree` fica implícito. Reusar o mesmo `--branch` num spawn seguinte
faz o checkout da branch existente (continua de onde parou) em vez de dar
erro. Os worktrees ficam em `<repo>/.agentmesh/worktrees/` — vale a pena
adicionar `.agentmesh/` ao `.gitignore` do repo.

`agentmesh ls` mostra a branch de cada agente isolado numa coluna própria.

**Ao encerrar, por padrão NADA é apagado** — `agentmesh kill` só mata a
sessão tmux; o worktree e a branch continuam lá pra você revisar/mergear
na mão:

```bash
agentmesh kill coder                                   # mantém tudo
agentmesh kill coder --remove-worktree                 # remove o worktree (recusa se tiver mudança não commitada)
agentmesh kill coder --remove-worktree --force          # remove mesmo com mudança não commitada
agentmesh kill coder --remove-worktree --delete-branch --force  # remove worktree E apaga a branch
```

## Diagnóstico: "⚠ precisa de atenção"

`agentmesh ls` marca com `⚠` qualquer agente cuja tela bata com um diálogo
bloqueante conhecido do Claude Code (confiança de pasta, menu de permissão)
— sinal de que ninguém confirmou aquilo ainda. `agentmesh attach NOME`
resolve na hora. (Hoje só reconhece os diálogos do Claude Code; outros
providers ainda não têm esse padrão mapeado.)

## Variáveis de ambiente

- `AGENTMESH_URL` — onde falar com o motor (default `http://127.0.0.1:8990`).
- `AGENTMESH_TERMINAL_ID` — identidade de quem está chamando a CLI; setada
  automaticamente nos agentes que o próprio `agentmesh spawn` cria.

## Custo (tokens/$) — diário e semanal

```bash
agentmesh usage             # hoje + últimos 7 dias, por dia e por modelo
agentmesh usage --days 30   # janela maior
```

Lê direto os transcripts que o **Claude Code já grava sozinho** em
`~/.claude/projects/**/*.jsonl` — não precisa do motor rodando, e cobre
TODO uso de Claude Code na máquina, não só os agentes que o agentmesh
spawnou. Custo é estimativa (tabela de preço fixa no código, não é
integração de billing) — direcional, não é fatura.

**Todo agente `claude` spawnado já nasce com isso no rodapé do próprio
terminal** (a barra de status do tmux, `agentmesh usage --oneline`,
atualizada sozinha a cada 20s pelo tmux) — não precisa abrir nada separado,
é só `agentmesh attach nome` e olhar o rodapé.

**codex/gemini/opencode ainda não têm isso** — cada um grava uso num
formato de log diferente e nenhum estava instalado nesta máquina pra eu
testar contra dado real antes de shipar (regra do projeto: nada entra sem
ter sido verificado contra o CLI de verdade). Fica como próximo passo
quando alguém tiver um desses configurado.

## Custo de recursos

Desprezível. Cada sessão tmux usa poucos MB de RAM e CPU irrelevante — o
que consome recursos de verdade é o processo do agente (`claude`/`codex`/
etc.) rodando dentro, exatamente igual a se você tivesse aberto ele à mão
num terminal comum. tmux não duplica processo nem adiciona nada pesado.

## O que ficou de fora (de propósito)

Isto é só o núcleo de comunicação. Não tem: canvas visual, kanban, notas,
roles/personas, spawn recursivo por agente, workspaces múltiplos, MCP
server. Tudo isso existe no [Openfield](https://github.com/NatanBack77/Openfield)
completo, que usa este mesmo desenho de motor por trás de uma GUI.
