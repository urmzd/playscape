.PHONY: all init build install test vet lint fmt check record

# The root library (with the playscape CLI) and the Kubernetes runner, which
# is its own module so importers of playscape do not inherit client-go.
MODULES := . pkg/runner/k8s

all: check

init:
	git config core.hooksPath .githooks
	@for m in $(MODULES); do (cd $$m && go mod download && go mod tidy) || exit 1; done

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/playscape ./cmd/playscape

install:
	CGO_ENABLED=0 go install -trimpath -ldflags="-s -w" ./cmd/playscape

test:
	@for m in $(MODULES); do (cd $$m && go test -race ./...) || exit 1; done

vet:
	@for m in $(MODULES); do (cd $$m && go vet ./...) || exit 1; done

lint:
	@for m in $(MODULES); do (cd $$m && golangci-lint run) || exit 1; done

fmt:
	gofmt -w .

check: fmt vet lint test

record:
	teasr showme
