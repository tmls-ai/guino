BINARY := guino
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)"

.PHONY: build test test-integration test-sdk e2e-network lint clean dashboard release release-check install docker

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/guino

test:
	go test ./cmd/guino/... ./internal/... ./pkg/... -short -v

test-integration:
	go test -tags integration ./internal/... ./tests/... -run TestIntegration -v

test-sdk:
	cd sdk/typescript && bun install --frozen-lockfile && bun test && bun run typecheck && bun run build
	cd sdk/python && uv sync --frozen --extra dev && uv run python -m pytest && uv build

# Machine-checkable end-to-end network proof (real guino binary + real Docker).
# Set GUINO_E2E_LOCAL_NATIVE=1 ONLY on native co-resident Linux for leg D.
e2e-network:
	./scripts/e2e-network.sh

# Pinned to golangci-lint v2.12.2 (matches .github/workflows/ci.yml). Install
# locally with the exact pin so local results match CI:
#   curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.12.2/install.sh \
#     | sh -s -- -b $(shell go env GOPATH)/bin v2.12.2
lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...

dashboard:
	cd dashboard-ui && bun install && bun run build
	cp -r dashboard-ui/dist/* internal/dashboard/dist/

clean:
	rm -rf bin/ internal/dashboard/dist/*.js internal/dashboard/dist/*.css

release:
	goreleaser release --snapshot --clean --skip=publish

release-check:
	python3 scripts/check-release.py --tag v0.1.0
	goreleaser check

install:
	go install ./cmd/guino

docker: docker-image

docker-image:
	docker build -t guino/default:latest images/default/

run: build
	./bin/$(BINARY) serve

all: lint test build
