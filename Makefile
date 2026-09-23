GOLANGCI_LINT_VERSION := v2.13.2
WORKFLOWCHECK_VERSION := v0.5.0
GOVULNCHECK_VERSION := v1.8.0
GOOSE_VERSION := v3.28.0

MIGRATIONS_DIR := internal/store/migrations
DATABASE_URL ?= postgres://conclave:conclave@localhost:5432/conclave?sslmode=disable

.PHONY: build test vet lint workflowcheck govulncheck check check-ci migrate dev down

build:
	go build ./...

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

workflowcheck:
	go run go.temporal.io/sdk/contrib/tools/workflowcheck@$(WORKFLOWCHECK_VERSION) ./...

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

check: vet lint test workflowcheck govulncheck

# Everything in `check` whose result depends on the diff. CI runs govulncheck
# separately, on main and on a schedule, because it tracks an external
# database rather than this repository.
check-ci: vet lint test workflowcheck

migrate:
	go run github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

dev:
	docker compose -f deploy/docker-compose.yml up -d

down:
	docker compose -f deploy/docker-compose.yml down
