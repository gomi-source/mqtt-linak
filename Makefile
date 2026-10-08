BIN_DIR   := bin
BINARY    := $(BIN_DIR)/mqtt-linak
CBGO_DIR  := ../corebluetooth-go
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X main.version=$(VERSION)

# LaunchAgent (macOS). Override on the command line, e.g.
#   make install-agent CONFIG=/path/to/config.yaml AGENT_LOG_LEVEL=debug
AGENT_LABEL     ?= se.gomi.mqtt-linak
AGENT_PLIST     := $(HOME)/Library/LaunchAgents/$(AGENT_LABEL).plist
AGENT_LOG       ?= $(HOME)/Library/Logs/mqtt-linak.log
AGENT_LOG_LEVEL ?= info
CONFIG          ?= $(CURDIR)/config.yaml
AGENT_DOMAIN    := gui/$(shell id -u)
AGENT_TARGET    := $(AGENT_DOMAIN)/$(AGENT_LABEL)

.PHONY: all build helper run test vet fmt tidy clean \
	install-agent uninstall-agent restart-agent agent-status agent-log

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

## --- LaunchAgent: start the bridge at login, restart it if it crashes ---
##
## install-agent builds bin/, writes $(AGENT_PLIST) from
## launchd/mqtt-linak.plist.in and loads it; it replaces an agent that is
## already loaded. The agent runs the binary in bin/ in place, so a later
## `make restart-agent` picks up a rebuild. Stop it with uninstall-agent,
## not by killing the process: that leaves the job loaded.

## Build, install the LaunchAgent and start it.
install-agent: all
	@command -v launchctl >/dev/null || { echo "install-agent needs macOS (launchctl)"; exit 1; }
	@test -f "$(CONFIG)" || { echo "no config at $(CONFIG); set CONFIG=/path/to/config.yaml"; exit 1; }
	mkdir -p "$(dir $(AGENT_PLIST))" "$(dir $(AGENT_LOG))"
	sed -e 's|@LABEL@|$(AGENT_LABEL)|g' \
	    -e 's|@BINARY@|$(CURDIR)/$(BINARY)|g' \
	    -e 's|@CONFIG@|$(abspath $(CONFIG))|g' \
	    -e 's|@LOG_LEVEL@|$(AGENT_LOG_LEVEL)|g' \
	    -e 's|@WORKDIR@|$(CURDIR)|g' \
	    -e 's|@LOG@|$(AGENT_LOG)|g' \
	    launchd/mqtt-linak.plist.in > "$(AGENT_PLIST)"
	plutil -lint "$(AGENT_PLIST)"
	@# bootout returns before the old job is gone, and bootstrapping over
	@# it fails with "Input/output error"; wait for it to disappear first.
	-@launchctl bootout $(AGENT_TARGET) 2>/dev/null; \
	for i in 1 2 3 4 5 6 7 8 9 10; do \
		launchctl print $(AGENT_TARGET) >/dev/null 2>&1 || break; sleep 0.5; \
	done
	launchctl bootstrap $(AGENT_DOMAIN) "$(AGENT_PLIST)"
	@echo "started $(AGENT_LABEL); log: $(AGENT_LOG)"
	@echo "the first start may ask for Bluetooth permission, and unpaired desks for pairing"

## Stop the agent and remove it; it will not start at the next login.
uninstall-agent:
	-launchctl bootout $(AGENT_TARGET) 2>/dev/null
	rm -f "$(AGENT_PLIST)"
	@echo "removed $(AGENT_LABEL)"

## Rebuild and restart the running agent.
restart-agent: all
	launchctl kickstart -k $(AGENT_TARGET)

## Whether the agent is loaded and running, its last exit code, and the
## end of its log.
agent-status:
	@launchctl print $(AGENT_TARGET) 2>/dev/null | grep -E '^[[:space:]]*(state|pid|last exit code|runs) =' || echo "$(AGENT_LABEL) is not loaded"
	@echo "--- $(AGENT_LOG)"
	@tail -n 20 "$(AGENT_LOG)" 2>/dev/null || echo "(no log yet)"

## Follow the agent's log.
agent-log:
	tail -f "$(AGENT_LOG)"
