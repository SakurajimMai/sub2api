#!/usr/bin/env bash
# 按当前源码重新生成 ent 与 wire 代码，保证生成文件与合并后的 fork + 上游源码一致。
# 生成工具通过 go run -mod=mod 带入的依赖不属于业务依赖，结束时还原 go.mod/go.sum。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT/backend"

tmp="$(mktemp -d)"
cp go.mod go.sum "$tmp/"
trap 'cp "$tmp/go.mod" "$tmp/go.sum" "$ROOT/backend/"; rm -rf "$tmp"' EXIT

# ent 先于 wire：wire 需要加载依赖 ent 的全部包
go generate ./ent
(cd cmd/server && go run -mod=mod github.com/google/wire/cmd/wire)
