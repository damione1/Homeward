#!/bin/sh
set -eu
# Runtime and generators are pinned together; tools are isolated in the ignored .tools folder.
mkdir -p .tools/bin
GOBIN="$PWD/.tools/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
cargo install protoc-gen-buffa --version 0.9.2 --locked --root "$PWD/.tools"
PATH="$PWD/.tools/bin:$PATH" buf generate
