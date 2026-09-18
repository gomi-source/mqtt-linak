BIN_DIR   := bin
BINARY    := $(BIN_DIR)/mqtt-linak
CBGO_DIR  := ../corebluetooth-go
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X main.version=$(VERSION)

.PHONY: all build helper run test vet fmt tidy clean

## Build the bridge and the CoreBluetooth helper it needs into bin/.
all: helper build

## Build the Go binary.
build:
	mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/mqtt-linak

## Build corebluetoothd from the sibling corebluetooth-go checkout and drop
## the .app bundle next to our binary, which is where ble.Start() looks for
## it by default. macOS ties CoreBluetooth authorization to the bundle
## identity, so the bundle - not a bare binary - is what has to be there.
helper:
	$(MAKE) -C $(CBGO_DIR) helper
	mkdir -p $(BIN_DIR)
	rm -rf $(BIN_DIR)/corebluetoothd.app
	cp -R $(CBGO_DIR)/helper/.build/corebluetoothd.app $(BIN_DIR)/corebluetoothd.app

## Run against ./config.yaml with debug logging.
run: all
	$(BINARY) -config config.yaml -log-level debug

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

## Resolve dependencies and write go.sum (needs network access).
tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)
