BINDIR  := bin
SBINDIR := /usr/local/sbin
CFGDIR  := /etc/lexa
SVCDIR  := /etc/systemd/system

SERVICES := hub northbound modbus ocpp telemetry
BINS     := $(addprefix $(BINDIR)/lexa-, $(SERVICES))

.PHONY: all build install install-configs install-services clean tidy test

all: build

build: $(BINS)

$(BINDIR)/lexa-%: cmd/%/*.go internal/**/*.go go.mod
	@mkdir -p $(BINDIR)
	go build -o $@ ./cmd/$*

# Cross-compile for Digi SOM (ARM64 Linux).
# Run on a machine with a proper cross toolchain; CGo is required for lexa-northbound
# and lexa-telemetry (wolfSSL). lexa-hub, lexa-ocpp, and lexa-modbus are
# pure Go and can be cross-compiled without CGo.
build-arm64:
	@mkdir -p $(BINDIR)/arm64
	GOARCH=arm64 GOOS=linux CGO_ENABLED=0 go build -o $(BINDIR)/arm64/lexa-hub      ./cmd/hub
	GOARCH=arm64 GOOS=linux CGO_ENABLED=0 go build -o $(BINDIR)/arm64/lexa-modbus   ./cmd/modbus
	GOARCH=arm64 GOOS=linux CGO_ENABLED=0 go build -o $(BINDIR)/arm64/lexa-ocpp     ./cmd/ocpp
	@echo "NOTE: lexa-northbound and lexa-telemetry require CGo (wolfSSL). Build on target or with cross toolchain."

# Install binaries (run on the target device as root)
install: build
	install -d $(SBINDIR)
	for svc in $(SERVICES); do \
		install -m 755 $(BINDIR)/lexa-$$svc $(SBINDIR)/lexa-$$svc; \
	done

# Install example configs (does not overwrite existing files)
install-configs:
	install -d $(CFGDIR)/certs
	for cfg in configs/*.json; do \
		dest=$(CFGDIR)/$$(basename $$cfg); \
		if [ ! -f $$dest ]; then install -m 640 $$cfg $$dest; fi \
	done
	@echo "Configs installed to $(CFGDIR) (existing files preserved)"

# Install and enable systemd services
install-services:
	install -m 644 systemd/lexa-*.service $(SVCDIR)/
	install -m 644 systemd/mosquitto-lexa.conf /etc/mosquitto/conf.d/lexa.conf
	systemctl daemon-reload
	systemctl enable mosquitto lexa-modbus lexa-northbound lexa-telemetry lexa-ocpp lexa-hub

# Start all services (after install-services)
start:
	systemctl start mosquitto lexa-modbus lexa-northbound lexa-telemetry lexa-ocpp lexa-hub

stop:
	systemctl stop lexa-hub lexa-ocpp lexa-telemetry lexa-northbound lexa-modbus

status:
	systemctl status lexa-hub lexa-northbound lexa-modbus lexa-ocpp lexa-telemetry --no-pager

logs:
	journalctl -f -u lexa-hub -u lexa-northbound -u lexa-modbus -u lexa-ocpp -u lexa-telemetry

tidy:
	go mod tidy

test:
	go test -race ./internal/...

clean:
	rm -rf $(BINDIR)
