BINDIR  := bin
SBINDIR := /usr/local/sbin
CFGDIR  := /etc/lexa
SVCDIR  := /etc/systemd/system

SERVICES := hub northbound modbus ocpp telemetry api
BINS     := $(addprefix $(BINDIR)/lexa-, $(SERVICES))

.PHONY: all build install install-configs install-services clean tidy test test-nocgo

all: build

build: $(BINS)

$(BINDIR)/lexa-%: cmd/%/*.go internal/**/*.go go.mod
	@mkdir -p $(BINDIR)
	go build -o $@ ./cmd/$*

# Cross-compile for Digi ConnectCore 93 (ARM64 Linux).
#
# Prerequisites (one-time setup):
#   sudo apt-get install -y gcc-aarch64-linux-gnu cmake automake autoconf libtool
#   make wolfssl-arm64          # builds + installs wolfSSL into WOLFSSL_SYSROOT
#
# Pure-Go services (no CGo):
GOARM64 := GOARCH=arm64 GOOS=linux CGO_ENABLED=0 go build

# CGo services (wolfSSL mTLS):
WOLFSSL_SYSROOT ?= /tmp/wolfssl-arm64-sysroot

# Host (amd64) wolfSSL sysroot so `make test`/`make build` work on machines
# without a system-wide wolfSSL (the desktop). No-op where the dir is absent.
# The static libwolfssl.a needs -lm (pow/log in dh.c).
WOLFSSL_SYSROOT_HOST ?= $(HOME)/.local/wolfssl-amd64
ifneq ($(wildcard $(WOLFSSL_SYSROOT_HOST)/include),)
export CGO_CFLAGS  += -I$(WOLFSSL_SYSROOT_HOST)/include
export CGO_LDFLAGS += -L$(WOLFSSL_SYSROOT_HOST)/lib -lm
endif
GOARM64_CGO := CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
	CC=aarch64-linux-gnu-gcc \
	CGO_CFLAGS="-I$(WOLFSSL_SYSROOT)/include" \
	CGO_LDFLAGS="-L$(WOLFSSL_SYSROOT)/lib -lwolfssl -lm" \
	go build

build-arm64:
	@mkdir -p $(BINDIR)/arm64
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-hub        ./cmd/hub
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-modbus     ./cmd/modbus
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-ocpp       ./cmd/ocpp
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-api        ./cmd/api
	$(GOARM64_CGO) -o $(BINDIR)/arm64/lexa-northbound ./cmd/northbound
	$(GOARM64_CGO) -o $(BINDIR)/arm64/lexa-telemetry  ./cmd/telemetry

# Build and install wolfSSL static library for ARM64 cross-compilation.
# Downloads wolfSSL 5.7.6, cross-compiles with aarch64-linux-gnu-gcc,
# and installs headers + libwolfssl.a into WOLFSSL_SYSROOT.
WOLFSSL_VER := 5.7.6-stable
wolfssl-arm64:
	@echo "Building wolfSSL $(WOLFSSL_VER) for ARM64 → $(WOLFSSL_SYSROOT)"
	@mkdir -p /tmp/wolfssl-build-arm64
	cd /tmp && wget -q https://github.com/wolfSSL/wolfssl/archive/refs/tags/v$(WOLFSSL_VER).tar.gz \
		-O wolfssl-$(WOLFSSL_VER).tar.gz && tar xzf wolfssl-$(WOLFSSL_VER).tar.gz
	cd /tmp/wolfssl-$(WOLFSSL_VER) && autoreconf -i
	cd /tmp/wolfssl-build-arm64 && /tmp/wolfssl-$(WOLFSSL_VER)/configure \
		--host=aarch64-linux-gnu CC=aarch64-linux-gnu-gcc \
		--prefix=$(WOLFSSL_SYSROOT) \
		--enable-tls13 --enable-aesccm --enable-tlsx \
		--enable-certgen --enable-opensslall \
		--enable-static --disable-shared \
		--disable-examples --disable-crypttests
	$(MAKE) -C /tmp/wolfssl-build-arm64 -j$$(nproc)
	$(MAKE) -C /tmp/wolfssl-build-arm64 install prefix=$(WOLFSSL_SYSROOT)
	@echo "wolfSSL installed to $(WOLFSSL_SYSROOT)"

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
	systemctl enable mosquitto lexa-modbus lexa-northbound lexa-telemetry lexa-ocpp lexa-hub lexa-api

# Start all services (after install-services)
start:
	systemctl start mosquitto lexa-modbus lexa-northbound lexa-telemetry lexa-ocpp lexa-hub lexa-api

stop:
	systemctl stop lexa-api lexa-hub lexa-ocpp lexa-telemetry lexa-northbound lexa-modbus

status:
	systemctl status lexa-hub lexa-northbound lexa-modbus lexa-ocpp lexa-telemetry lexa-api --no-pager

logs:
	journalctl -f -u lexa-hub -u lexa-northbound -u lexa-modbus -u lexa-ocpp -u lexa-telemetry -u lexa-api

tidy:
	go mod tidy

test:
	go test -race ./internal/...

# Mirrors CI's vet-build-test job: -race over every package that does NOT
# import internal/wolfssl or internal/tlsclient (the cgo boundary), across
# both ./internal/... and ./cmd/.... Needs no wolfSSL headers — runs
# anywhere, including hosted CI runners. See .github/workflows/ci.yml.
test-nocgo:
	go test -race $(shell go list ./internal/... | grep -v -e internal/wolfssl -e internal/tlsclient)
	go test -race $(shell go list ./cmd/... | grep -v -e cmd/northbound -e cmd/telemetry)

clean:
	rm -rf $(BINDIR)
