BINARY := bin/receipts-mcp
PKG := ./cmd/receipts-mcp

.PHONY: build test vet fmt check clean run-status

build:
	go build -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

# check is what CI and a pre-commit run: formatting, vet, tests with the race detector.
check: vet
	@unformatted=$$(gofmt -l ./cmd ./internal); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go test -race ./...

run-status: build
	$(BINARY) -status

clean:
	rm -rf bin
