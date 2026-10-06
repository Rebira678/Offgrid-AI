#!/usr/bin/env bash
# Download the models used by the Offgrid benchmark.
# Usage: scripts/pull-models.sh                 (the two small models, about 3.3 GB)
#        scripts/pull-models.sh llama3.2:3b     (only what you name)
#        scripts/pull-models.sh --with-7b       (also a 7B model, about 4.7 GB more)
#
# Models are stored by Ollama on your disk, so each is downloaded only once.
set -euo pipefail

if ! command -v ollama >/dev/null 2>&1; then
  echo "ollama is not installed. Install it with:" >&2
  echo "  curl -fsSL https://ollama.com/install.sh | sh" >&2
  exit 1
fi

models=()
for arg in "$@"; do
  case "$arg" in
    --with-7b) models+=("llama3.2:1b" "llama3.2:3b" "qwen2.5:7b") ;;
    *)         models+=("$arg") ;;
  esac
done
if [ ${#models[@]} -eq 0 ]; then
  models=("llama3.2:1b" "llama3.2:3b")
fi

for m in "${models[@]}"; do
  echo "==> pulling $m"
  ollama pull "$m"
done

echo
ollama list
