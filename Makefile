BIN     = xl710-unlock
GOFLAGS = -trimpath -ldflags='-s -w'

# Static Linux binaries; build on any machine, copy to the box with the NIC.
all: $(BIN)-linux-amd64 $(BIN)-linux-arm64

$(BIN)-linux-%: *.go go.mod
	CGO_ENABLED=0 GOOS=linux GOARCH=$* go build $(GOFLAGS) -o $@ .

test:
	go vet ./...
	GOOS=linux go vet ./...
	go test ./...

clean:
	rm -f $(BIN)-linux-*

.PHONY: all test clean
