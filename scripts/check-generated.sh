#!/bin/sh
set -eu
mkdir -p .tools/bin
if [ "$(.tools/bin/protoc-gen-go --version 2>/dev/null || true)" != "protoc-gen-go v1.36.11" ]; then
  GOBIN="$PWD/.tools/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
fi
if [ "$(.tools/bin/protoc-gen-connect-go --version 2>/dev/null || true)" != "1.21.0" ]; then
  GOTOOLCHAIN=local GOBIN="$PWD/.tools/bin" go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.0
fi
if [ "$(.tools/bin/protoc-gen-buffa --version 2>/dev/null || true)" != "protoc-gen-buffa 0.9.2" ]; then
  cargo install protoc-gen-buffa --version 0.9.2 --locked --root "$PWD/.tools"
fi
contract_temp="$(mktemp -d)"
trap 'rm -rf "$contract_temp"' EXIT HUP INT TERM
cat > "$contract_temp/buf.gen.yaml" <<EOF
version: v2
plugins:
  - local: protoc-gen-go
    out: $contract_temp/go
    opt: paths=source_relative
  - local: protoc-gen-connect-go
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
echo "Go/Connect/Rust generated bindings match pinned generators"
