#!/bin/sh
set -eu
mkdir -p .tools/worker-python
if [ -n "${HOMEWARD_NHA094_PYTHON:-}" ]; then
  worker_python="$HOMEWARD_NHA094_PYTHON"
elif [ -x "$HOME/Library/Caches/codex-build/homeward/worker-python/venv/bin/python" ]; then
  worker_python="$HOME/Library/Caches/codex-build/homeward/worker-python/venv/bin/python"
else
  worker_python="$PWD/.tools/worker-python/venv/bin/python"
  if [ ! -x "$worker_python" ]; then
    [ "$(uv --version | awk '{print $2}')" = "0.10.7" ]
    UV_CACHE_DIR="$PWD/.tools/worker-python/uv-cache" uv venv .tools/worker-python/venv --python python3 --no-managed-python
    UV_CACHE_DIR="$PWD/.tools/worker-python/uv-cache" uv pip sync --python "$worker_python" --only-binary :all: testworker/requirements.lock
  fi
fi
"$worker_python" -c 'import grpc,google.protobuf; import grpc_tools.protoc; assert grpc.__version__ == "1.75.1" and google.protobuf.__version__ == "6.32.1"'
printf '%s\n' "$worker_python" > .tools/worker-python/python-path
