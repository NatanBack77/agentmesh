#!/usr/bin/env bash
# Instala o agentmesh na máquina atual: compila o binário e coloca em
# ~/.local/bin/agentmesh. Rode a partir da raiz do repo:
#   ./scripts/install.sh
set -euo pipefail

if ! command -v go >/dev/null 2>&1; then
  echo "agentmesh: preciso do Go instalado (https://go.dev/dl/) — não encontrei 'go' no PATH." >&2
  exit 1
fi

cd "$(dirname "${BASH_SOURCE[0]}")/.."

mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/agentmesh" ./cmd/agentmesh
echo "agentmesh: instalado em $HOME/.local/bin/agentmesh"

case ":$PATH:" in
  *":$HOME/.local/bin:"*) ;;
  *)
    echo
    echo "aviso: ~/.local/bin não está no seu PATH. Adicione ao seu ~/.bashrc ou ~/.zshrc:"
    echo '  export PATH="$HOME/.local/bin:$PATH"'
    ;;
esac

echo
echo "pronto. teste com:"
echo "  agentmesh serve &"
echo "  agentmesh spawn shell bash"
echo "  agentmesh ls"
