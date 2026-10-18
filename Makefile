.PHONY: check
check:
	sh scripts/ensure-worker-python.sh
	test -z "$$(gofmt -l cmd internal contracts compat)"
	go vet ./...
	go test -race ./...
	cargo fmt --all -- --check
	cargo clippy --locked --workspace --all-targets -- -D warnings
	cargo test --locked --workspace
	cargo check --locked --workspace
	buf lint
	python3 scripts/check-boundaries.py
	python3 scripts/smoke-bootstrap.py
	python3 scripts/check-corpus.py
	python3 scripts/check-scope.py
	python3 scripts/test-scope.py
	buf breaking --against .compat/baseline
	python3 scripts/check-contracts.py
	sh scripts/check-generated.sh
	sh scripts/check-worker-generated.sh
	python3 scripts/check-matter.py
