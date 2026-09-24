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

## Bundle corebluetoothd into bin/, where ble.Start() looks for it by
## default. Prefers a corebluetoothd already on this machine (Homebrew
## tap, or a GitHub Release .app you've dropped into $(CBGO_DIR)/bin) over
## building one from the sibling corebluetooth-go checkout, so this repo
## doesn't need its own Xcode/Swift toolchain just to bundle a helper
## that's already installed. Falls back to building it if neither is
## found. macOS ties CoreBluetooth authorization to the bundle identity,
## so the .app bundle - not a bare binary - is what has to end up here.
helper:
	mkdir -p $(BIN_DIR)
	rm -rf $(BIN_DIR)/corebluetoothd.app
	@if command -v corebluetoothd >/dev/null 2>&1; then \
		real="$$(readlink -f "$$(command -v corebluetoothd)")"; \
		src="$${real%/Contents/MacOS/corebluetoothd}"; \
		echo "using installed corebluetoothd: $$src"; \
		cp -R "$$src" $(BIN_DIR)/corebluetoothd.app; \
	elif [ -d $(CBGO_DIR)/bin/corebluetoothd.app ]; then \
		echo "using $(CBGO_DIR)/bin/corebluetoothd.app"; \
		cp -R $(CBGO_DIR)/bin/corebluetoothd.app $(BIN_DIR)/corebluetoothd.app; \
	else \
		echo "no installed corebluetoothd found; building from $(CBGO_DIR)"; \
		$(MAKE) -C $(CBGO_DIR) helper; \
		cp -R $(CBGO_DIR)/helper/.build/corebluetoothd.app $(BIN_DIR)/corebluetoothd.app; \
	fi

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
