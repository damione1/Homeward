.DEFAULT_GOAL := check
.PHONY: check package-server smoke-compose

package-server:
	python3 scripts/package-server.py

smoke-compose:
	python3 scripts/smoke-compose.py

check: export PYTHONDONTWRITEBYTECODE=1
check:
	sh scripts/ensure-worker-python.sh
	PYTHONDONTWRITEBYTECODE=1 "$$(cat .tools/worker-python/python-path)" -m unittest -q testworker.caseta_worker_test
	test -z "$$(gofmt -l cmd internal contracts compat sdk examples scope qualification/fido2)"
	test -z "$$(gofmt -l qualification/ecobee/fixture_test.go qualification/ecobee/identity_test.go qualification/ecobee/lifecycle_test.go qualification/ecobee/poll.go qualification/ecobee/poll_test.go qualification/ecobee/profile.go qualification/ecobee/profile_test.go)"
	go vet ./...
	go test -race ./...
	cd qualification/fido2 && go test -race -count=1 ./...
	cd qualification/ecobee && go test -race -mod=readonly -count=1 ./...
	cd third_party/homekit && go test -race -mod=readonly -count=1 ./...
	cd examples/conformance-consumer && go test -race -mod=readonly ./... -run '^TestNHA095' -count=1
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
