.PHONY: build test test-race fmt fmt-check vet lint vuln python-free verify

GOLANGCI_LINT_VERSION ?= v2.13.2
GOVULNCHECK_VERSION ?= v1.8.0

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/kedr ./cmd/kedr

test:
	go test ./...

test-race:
	go test -race ./...

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

fmt-check:
	test -z "$$(gofmt -l .)"

vet:
	go vet ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

python-free:
	test -z "$$(find . -name '*.py' -o -name 'requirements.txt' -o -name 'poetry.lock')"

verify: fmt-check vet test-race lint vuln python-free
