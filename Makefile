GOLANGCI_LINT_VERSION := v2.14.0
WORKFLOWCHECK_VERSION := v0.5.0
GOVULNCHECK_VERSION := v1.8.0
GOOSE_VERSION := v3.28.0

MIGRATIONS_DIR := internal/store/migrations
DATABASE_URL ?= postgres://conclave:conclave@localhost:5432/conclave?sslmode=disable

.PHONY: build test test-ci vet lint tidy workflowcheck govulncheck check check-ci migrate dev down

build:
	go build ./...

test:
	go test -race ./...

# CI runs every test fresh. The restored build cache also holds test results,
# and Go keys those on the test binary, the environment variables, and the
# files a test reads, not on the Docker daemon or kernel underneath. Without
# -count=1 the sandbox attack tests replay a past pass. The variable turns
# testenv.Skip into a failure, so a security test cannot pass by not running.
test-ci:
	CONCLAVE_REQUIRE_INTEGRATION=1 go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

# go.mod and go.sum must already be tidy, and the module cache must match
# go.sum, so a dependency never changes without showing up in review.
tidy:
	go mod tidy -diff
	go mod verify

workflowcheck:
	go run go.temporal.io/sdk/contrib/tools/workflowcheck@$(WORKFLOWCHECK_VERSION) ./...

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

check: vet lint tidy test workflowcheck govulncheck

# Everything in `check` whose result depends on the diff, with tests run as
# test-ci. CI runs govulncheck separately, on main and on a schedule, because
# it tracks an external database rather than this repository.
check-ci: vet lint tidy test-ci workflowcheck

migrate:
	go run github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

dev:
	docker compose -f deploy/docker-compose.yml up -d

down:
	docker compose -f deploy/docker-compose.yml down
