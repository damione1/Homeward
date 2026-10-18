#!/bin/sh
set -eu
mkdir -p .tools/bin
GOBIN="$PWD/.tools/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
cargo install protoc-gen-buffa --version 0.9.2 --locked --root "$PWD/.tools"
contract_temp="$(mktemp -d)"
trap 'rm -rf "$contract_temp"' EXIT HUP INT TERM
cat > "$contract_temp/buf.gen.yaml" <<EOF
version: v2
plugins:
  - local: protoc-gen-go
    out: $contract_temp/go
    opt: paths=source_relative
  - local: protoc-gen-buffa
    strategy: all
    out: $contract_temp/rust
    opt: [json=false, file_per_package=true]
EOF
PATH="$PWD/.tools/bin:$PATH" buf generate --template "$contract_temp/buf.gen.yaml"
diff -r gen "$contract_temp/go"
diff -r client/wire/src/generated "$contract_temp/rust"
echo "Go/Rust generated bindings match pinned generators"
