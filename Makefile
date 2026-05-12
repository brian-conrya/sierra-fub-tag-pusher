BIN          := bin/pusher
PKG          := ./...
GOFLAGS      ?=
GOLANGCI     ?= golangci-lint

.PHONY: all build run test test-race cover vet lint fmt tidy clean ci

all: fmt vet lint test build

build:
	go build $(GOFLAGS) -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/pusher

run:
	go run ./cmd/pusher

test:
	go test $(GOFLAGS) -count=1 $(PKG)

test-race:
	go test $(GOFLAGS) -count=1 -race $(PKG)

cover:
	go test $(GOFLAGS) -count=1 -covermode=atomic -coverprofile=coverage.txt $(PKG)
	go tool cover -func=coverage.txt | tail -1

vet:
	go vet $(PKG)

lint:
	$(GOLANGCI) run

fmt:
	gofmt -w -s .

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.txt

ci: tidy fmt vet lint test-race build
