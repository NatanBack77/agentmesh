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

Um único processo (`agentmesh serve`) mantém um PTY real por agente
spawnado, detecta quando cada um termina o turno (regex sobre a saída —
funciona com qualquer CLI, sem precisar que o agente coopere) e expõe uma
API HTTP local (loopback, `127.0.0.1`) com as primitivas de comunicação.
Todo agente registrado é peer de todos os outros — não existe passo de
"desenhar uma seta" como no Openfield: é uma malha (mesh) plana.

## Instalar

```bash
go build -o ~/.local/bin/agentmesh ./cmd/agentmesh
```

## Uso

```bash
# 1. sobe o motor (deixe rodando num terminal, ou em background)
agentmesh serve &

# 2. spawna os agentes
agentmesh spawn coder    claude --cwd ~/meu-projeto
agentmesh spawn reviewer claude --cwd ~/meu-projeto

# 3. lista quem está rodando
agentmesh ls

# 4. entra interativamente num agente (terminal de verdade, raw mode)
agentmesh attach coder          # Ctrl+] desanexa sem matar o processo

# 5. de outro terminal, ou de dentro de um dos agentes via Bash tool:
agentmesh send reviewer "dá uma olhada no PR aberto"        # não bloqueia
agentmesh handoff coder "implementa X" --timeout 300        # bloqueia e traz o resultado
agentmesh exec meushell "npm test"                           # só em agentes shell

agentmesh whoami                # identidade do agente atual
agentmesh watch reviewer        # acompanha a saída, só leitura
agentmesh kill reviewer
```

Quando o agente spawnado é `claude`, o `agentmesh` já injeta via
`--append-system-prompt` a instrução de que ele pode usar a CLI `agentmesh`
(disponível no PATH, com `AGENTMESH_TERMINAL_ID`/`AGENTMESH_URL` já no
ambiente) através da ferramenta Bash para falar com os outros agentes —
então um `claude` pode chamar `agentmesh peers` / `agentmesh send` /
`agentmesh handoff` sozinho, sem intervenção sua. Para qualquer provider
(inclusive shells puros), o motor também escreve `.agentmesh/peers.md` no
diretório de trabalho de cada agente — uma lista simples de quem mais está
rodando, legível por qualquer ferramenta de leitura de arquivo.

## Primitivas

- **`send`** — entrega a mensagem agora (se o agente estiver ocioso) ou
  enfileira (se estiver processando); não bloqueia quem chamou.
- **`handoff`** — entrega a mensagem, espera o agente terminar o turno, e
  devolve o que ele produziu. Bloqueante, com timeout configurável e
  detecção de deadlock/ciclo na cadeia de delegação.
- **`exec`** — igual a `handoff`, mas só aceita alvos que sejam shells
  puros (`bash`/`zsh`/`sh`/`fish`) — é a forma seguro de um agente rodar um
  comando "no terminal visível do usuário" sem escrever na entrada de
  outro agente.

## Variáveis de ambiente

- `AGENTMESH_URL` — onde falar com o motor (default `http://127.0.0.1:8990`).
- `AGENTMESH_TERMINAL_ID` — identidade de quem está chamando a CLI; setada
  automaticamente nos agentes que o próprio `agentmesh spawn` cria.
- `AGENTMESH_SOCK` — caminho do socket Unix usado por `attach`/`watch`
  (default `$XDG_RUNTIME_DIR/agentmesh.sock`).

## O que ficou de fora (de propósito)

Isto é só o núcleo de comunicação. Não tem: canvas visual, kanban, notas,
roles/personas, spawn recursivo por agente, workspaces múltiplos, MCP
server. Tudo isso existe no [Openfield](https://github.com/NatanBack77/Openfield)
completo, que usa este mesmo desenho de motor por trás de uma GUI.
