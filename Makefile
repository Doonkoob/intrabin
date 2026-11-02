VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/intrabin/intrabin/internal/node.Version=$(VERSION) -X github.com/intrabin/intrabin/internal/node.Commit=$(COMMIT) -X github.com/intrabin/intrabin/internal/node.BuildDate=$(DATE)

.PHONY: all web build test test-go test-e2e test-cluster test-cluster-compose lint run docker clean

all: build

web:
	cd apps/web && npm ci --no-audit --no-fund && npx tsc -b && npx vite build
	touch internal/web/dist/.gitkeep

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/intrabin ./cmd/intrabin

test: test-go

test-go:
	go vet ./...
	go test -race -count=1 ./...

test-e2e: build
	cd apps/web && npx playwright test

test-cluster: build
	tests/cluster/run_local.sh

test-cluster-compose:
	tests/cluster/run_compose.sh

run: build
	INTRABIN_DATA_DIR=./data INTRABIN_LOG_FORMAT=text ./bin/intrabin

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_DATE=$(DATE) -t intrabin/intrabin:$(VERSION) -t intrabin/intrabin:latest .

clean:
	rm -rf bin data apps/web/test-results
