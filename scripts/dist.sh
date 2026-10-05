#!/bin/sh
# Builds archives for Linux, macOS and Windows: make dist
set -eu
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)}
OUT=dist
rm -rf "$OUT" && mkdir -p "$OUT"

crlf() { sed 's/$/\r/' "$1" > "$2"; } # for Notepad and cmd.exe

for target in windows/amd64 windows/arm64 darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  os=${target%/*} arch=${target#*/}
  name="spendbot-$VERSION-$( [ "$os" = darwin ] && echo macos || echo "$os" )-$arch"
  dir="$OUT/$name"
  mkdir -p "$dir"
  exe=spendbot; [ "$os" = windows ] && exe=spendbot.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "$dir/$exe" ./cmd/spendbot
  case $os in
    windows)
      crlf deploy/dist/spendbot.env "$dir/spendbot.env"
      crlf deploy/dist/start.bat "$dir/start.bat"
      crlf deploy/dist/README.txt "$dir/README.txt" # ASCII name: zip on a Mac mangles non-ASCII file names
      (cd "$OUT" && zip -qr "$name.zip" "$name") ;;
    darwin)
      cp deploy/dist/spendbot.env deploy/dist/README.txt "$dir/"
      cp deploy/dist/start.command "$dir/" && chmod +x "$dir/start.command"
      (cd "$OUT" && zip -qr "$name.zip" "$name") ;;
    linux)
      cp deploy/dist/spendbot.env deploy/dist/README.txt "$dir/"
      tar -C "$OUT" -czf "$OUT/$name.tar.gz" "$name" ;;
  esac
  rm -rf "$dir"
  echo "$OUT/$name"
done
