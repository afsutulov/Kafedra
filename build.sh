#!/bin/sh
# Сборка ПК «Кафедра». Требуется только инструментарий Go 1.24+; доступ в сеть не нужен:
# сторонних модулей у проекта нет.
set -eu
cd "$(dirname "$0")"
VERSION="${VERSION:-1.3.1}"
export CGO_ENABLED=0 GOFLAGS=-mod=mod GOPROXY=off
# защита комплекта поставки: служебные файлы не должны быть пустыми
for f in deploy/kafedra.service deploy/install.sh; do [ -s "$f" ] || { echo "Пустой файл: $f"; exit 1; }; done
go vet ./...
go test ./...
mkdir -p dist
for ARCH in amd64 arm64; do
  GOOS=linux GOARCH=$ARCH go build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "dist/kafedra-linux-$ARCH" ./cmd/kafedra
done
(cd dist && sha256sum kafedra-linux-* > SHA256SUMS)
echo "Готово: dist/"; cat dist/SHA256SUMS
