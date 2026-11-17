.DEFAULT_GOAL := check
GO_TEST_TIMEOUT ?= 15m
.PHONY: check package-server smoke-compose qualify-mvp-software

package-server:
	@test -n "$(PLATFORM)" || { echo "Set PLATFORM=linux/amd64 or PLATFORM=linux/arm64" >&2; exit 1; }
	python3 scripts/package-server.py --platform "$(PLATFORM)"

smoke-compose:
	python3 scripts/smoke-compose.py

check: export PYTHONDONTWRITEBYTECODE=1
# A debug build of the generated `HomewardWindow::new` keeps two copies of the
# window struct on the stack (about 1 MB of frame), more than the 2 MB a test
# thread gets by default; every homeward-app test that builds the window needs
# the larger stack. A direct `cargo test` needs the same variable (or a local
# .cargo/config.toml `[env]` entry).
check: export RUST_MIN_STACK ?= 8388608
check:
	sh scripts/ensure-worker-python.sh
	PYTHONDONTWRITEBYTECODE=1 "$$(cat .tools/worker-python/python-path)" -m unittest -q testworker.caseta_worker_test testworker.esphome_worker_test
	test -z "$$(gofmt -l cmd internal contracts compat sdk examples scope qualification/fido2)"
	test -z "$$(gofmt -l qualification/ecobee/fixture_test.go qualification/ecobee/identity_test.go qualification/ecobee/lifecycle_test.go qualification/ecobee/poll.go qualification/ecobee/poll_test.go qualification/ecobee/profile.go qualification/ecobee/profile_test.go)"
	go vet ./...
	go test -race -timeout=$(GO_TEST_TIMEOUT) ./...
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
	python3 scripts/check-contracts.py
	sh scripts/check-generated.sh
	sh scripts/check-worker-generated.sh
	python3 scripts/check-matter.py
	python3 scripts/test-frontend.py
	python3 scripts/check-frontend.py

# PROFILE=preflight is explicitly shorter and never qualifies the required hour.
PROFILE ?= acceptance
qualify-mvp-software:
	test -n "$(PROOF)"
	HOMEWARD_NHA165_PROFILE="$(PROFILE)" HOMEWARD_NHA165_PROOF="$(PROOF)" go test -v -count=1 -timeout=75m ./internal/server -run '^TestNHA165SoftwareProfile$$'

.PHONY: frontend-desktop frontend-android check-frontend qualify-frontend-transport
frontend-desktop:
	python3 scripts/frontend.py run --cargo -- python3 scripts/package-frontend-desktop.py

frontend-android:
	python3 scripts/frontend.py run --cargo -- python3 scripts/build-frontend-android.py

check-frontend:
	python3 scripts/frontend.py run --cargo -- python3 scripts/check-frontend.py

qualify-frontend-transport:
	python3 scripts/frontend.py run --cargo -- python3 scripts/qualify-frontend-transport.py

.PHONY: qualify-frontend-session
qualify-frontend-session:
	python3 scripts/frontend.py run --cargo -- python3 scripts/qualify-frontend-session.py
