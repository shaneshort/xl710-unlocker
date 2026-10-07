BIN     = xl710-unlock
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS = -trimpath -ldflags='-s -w -X main.version=$(VERSION)'

# Static Linux binary; build on any machine, copy to the box with the NIC.
all: $(BIN)-linux-amd64

$(BIN)-linux-%: *.go go.mod
	CGO_ENABLED=0 GOOS=linux GOARCH=$* go build $(GOFLAGS) -o $@ .

test:
	go vet ./...
	GOOS=linux go vet ./...
	go test ./...

lint:
	golangci-lint run ./...
	GOOS=darwin golangci-lint run ./...

clean:
	rm -f $(BIN)-linux-*

.PHONY: all test lint clean
