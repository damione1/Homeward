#!/bin/sh
set -eu
worker_python="$(cat .tools/worker-python/python-path)"
worker_temp="$(mktemp -d)"
trap 'rm -rf "$worker_temp"' EXIT HUP INT TERM
"$worker_python" -m grpc_tools.protoc -Iproto --python_out="$worker_temp" --grpc_python_out="$worker_temp" \
  proto/home/registry/v1/registry.proto proto/home/state/v1/state.proto \
  proto/home/actions/v1/actions.proto proto/home/integrations/v1/host.proto
diff -r testworker/generated "$worker_temp"
python3 scripts/host-schema.py --check
