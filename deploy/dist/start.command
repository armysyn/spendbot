#!/bin/sh
# Double-click in Finder to run spendbot from this folder.
cd "$(dirname "$0")"
# macOS marks downloaded files; without this Gatekeeper refuses the unsigned binary.
xattr -d com.apple.quarantine ./spendbot 2>/dev/null
if ! command -v ollama >/dev/null 2>&1 && [ ! -d /Applications/Ollama.app ]; then
  echo "Ollama was not found, so the local model will not work. Get it at https://ollama.com/download"
fi
exec ./spendbot
