BINDIR  := bin
SBINDIR := /usr/local/sbin
CFGDIR  := /etc/lexa
SVCDIR  := /etc/systemd/system

# VERSION (GAP-5, internal/buildinfo): the build-version string stamped into
# internal/buildinfo.Version via -ldflags -X, surfaced today by lexa-api's
# mDNS TXT "fw=", GET /site.fw, and GET /status.fw. Override on the command
# line for a real build, e.g. `make build-arm64 VERSION=1.2.3`; the "dev"
# default matches buildinfo.Version's own unstamped default, so a plain
# `make build`/`go build` and a `VERSION=dev` build are indistinguishable.
#
# LDFLAGS is applied to EVERY service build below, not just lexa-api's,
# even though internal/buildinfo.Version is (today) only read by lexa-api.
# This was a deliberate simplicity choice over scoping the flag to a single
# $(BINDIR)/lexa-api rule carved out of the generic pattern rule: `-X` on a
# symbol path a given binary doesn't happen to import is a silent no-op
# (verified — no warning, no error, `go build -ldflags "-X
# lexa-hub/internal/buildinfo.Version=x" ./cmd/healthcheck` links clean), so
# there is no cost to the five services that don't read it today, and any
# future service that starts reading buildinfo.Version gets the stamp for
# free with no Makefile change.
VERSION ?= dev
LDFLAGS := -X lexa-hub/internal/buildinfo.Version=$(VERSION)

# healthcheck is not a service (no unit file, no daemon) but builds/installs
# through the same pattern rule: bin/lexa-healthcheck → /usr/local/sbin, the
# path scripts/mender/ArtifactCommit_Enter_00_lexa-health expects (unit 1.5).
SERVICES := hub northbound modbus ocpp telemetry api healthcheck cloudlink provision openadr
BINS     := $(addprefix $(BINDIR)/lexa-, $(SERVICES))

.PHONY: all build install install-configs install-services clean tidy test test-nocgo contract fuzz sweep-sunspec

all: build

build: $(BINS) $(BINDIR)/lexa-migrate $(BINDIR)/lexactl

$(BINDIR)/lexa-%: cmd/%/*.go internal/**/*.go go.mod
	@mkdir -p $(BINDIR)
	go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/$*

# lexa-migrate's source dir (cmd/lexa-migrate) doesn't fit the lexa-% pattern
# rule above (% would resolve to cmd/migrate), so it gets an explicit rule.
# Oneshot config-schema migrator (unit 1.6) — pure Go, runs before the
# services via systemd/lexa-migrate.service.
$(BINDIR)/lexa-migrate: cmd/lexa-migrate/*.go go.mod
	@mkdir -p $(BINDIR)
	go build -o $@ ./cmd/lexa-migrate

# lexactl is the operator CLI (unit 7.1): binary is named `lexactl` (what a
# user types), so the lexa-% pattern rule can't serve it either.
$(BINDIR)/lexactl: cmd/lexactl/*.go go.mod
	@mkdir -p $(BINDIR)
	go build -o $@ ./cmd/lexactl

# Cross-compile for Digi ConnectCore 93 (ARM64 Linux).
#
# Prerequisites (one-time setup):
#   sudo apt-get install -y gcc-aarch64-linux-gnu cmake automake autoconf libtool
#   make wolfssl-arm64          # builds + installs wolfSSL into WOLFSSL_SYSROOT
#
# Pure-Go services (no CGo):
GOARM64 := GOARCH=arm64 GOOS=linux CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)"

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
	go build -ldflags "$(LDFLAGS)"

build-arm64:
	@mkdir -p $(BINDIR)/arm64
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-hub        ./cmd/hub
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-modbus     ./cmd/modbus
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-ocpp       ./cmd/ocpp
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-api        ./cmd/api
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-healthcheck ./cmd/healthcheck
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-migrate     ./cmd/lexa-migrate
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-cloudlink   ./cmd/cloudlink
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-provision   ./cmd/provision
	$(GOARM64)     -o $(BINDIR)/arm64/lexa-openadr     ./cmd/openadr
	$(GOARM64)     -o $(BINDIR)/arm64/lexactl          ./cmd/lexactl
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
	install -m 755 $(BINDIR)/lexa-migrate $(SBINDIR)/lexa-migrate
	install -m 755 $(BINDIR)/lexactl $(SBINDIR)/lexactl

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
	install -m 644 systemd/lexa-commission.path $(SVCDIR)/
	install -m 755 scripts/lexa-commission-apply $(SBINDIR)/lexa-commission-apply
	install -m 644 systemd/mosquitto-lexa.conf /etc/mosquitto/conf.d/lexa.conf
	systemctl daemon-reload
	systemctl enable mosquitto lexa-migrate lexa-commission.path lexa-modbus lexa-northbound lexa-telemetry lexa-ocpp lexa-hub lexa-api lexa-cloudlink lexa-openadr

# Start all services (after install-services)
start:
	systemctl start mosquitto lexa-modbus lexa-northbound lexa-telemetry lexa-ocpp lexa-hub lexa-api lexa-cloudlink lexa-openadr

stop:
	systemctl stop lexa-openadr lexa-cloudlink lexa-api lexa-hub lexa-ocpp lexa-telemetry lexa-northbound lexa-modbus

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

# Workstream C: the hub⇄app HTTP contract drift gate. cmd/api/contract_test.go
# drives the REAL /status, /site, /devices, /telemetry, /mode, /plan, /scan,
# /intent handlers and asserts each still conforms to the pinned golden
# fixtures in internal/apicontract/http_v1/ (checker + policy:
# internal/apicontract, docs/API_CONTRACT.md). cmd/api is pure-Go, so this is
# already covered transitively by test-nocgo above; this named target exists so
# the gate can be invoked (and required in CI) on its own. See
# .github/workflows/ci.yml's contract step.
contract:
	go test ./cmd/api/...

# TASK-053 (GAP-07): quick local re-run of just the int16/scale-factor
# boundary sweep — the shared lexa-proto/sunspec codec contract (against
# this repo's vendored copy) plus the two watt->ActivePower encoders'
# encode-scaling + cross-encoder-agreement checks. Already covered by
# `test-nocgo` / CI; this target is for a fast local loop while iterating.
sweep-sunspec:
	go test -race -run 'Sweep' ./internal/southbound/sunspecsweep/...
	go test -race -run 'WattsToActivePower' ./cmd/hub/...
	go test -race -run 'ActivePowerFromWatts' ./cmd/modbus/...

clean:
	rm -rf $(BINDIR)

# TASK-047: 15 minutes per go-native fuzz target against the CGo-free
# httpwire leaf package (the extracted core of the former
# tlsclient.readResponse — header cap, Content-Length handling, chunked
# rejection). No wolfSSL sysroot needed: httpwire imports stdlib only,
# so this runs on any machine, including hosted CI runners (see the
# nightly `fuzz` job in .github/workflows/ci.yml). Merge gate for
# TASK-047 is these three runs clean; failures land a crash file under
# internal/tlsclient/httpwire/testdata/fuzz/<FuzzName>/ that `go test`
# (no -fuzz) reruns forever after as a regression case.
#
# TASK-048 extends this same target with the other two untrusted decode
# surfaces: the 2030.5 XML unmarshal path (internal/northbound/scheduler —
# the model package these targets used to live in, internal/northbound/model,
# was merged into lexa-proto/csipmodel by TASK-023; the fuzz targets moved to
# the consumer that owns the downstream plausibility gate they drive) and the
# bus JSON decode path (internal/bus, mirroring mqttutil.Subscribe[T]'s
# CheckVersion + json.Unmarshal sequence). Same CGo-free, any-runner
# properties as httpwire. Crashers land under each package's own
# testdata/fuzz/<FuzzName>/, same regression-replay behavior as above.
FUZZTIME ?= 15m
fuzz:
	go test -fuzz=FuzzReadHTTPResponse      -fuzztime=$(FUZZTIME) ./internal/tlsclient/httpwire/
	go test -fuzz=FuzzResponseContentLength -fuzztime=$(FUZZTIME) ./internal/tlsclient/httpwire/
	go test -fuzz=FuzzIsChunkedEncoding      -fuzztime=$(FUZZTIME) ./internal/tlsclient/httpwire/
	go test -fuzz=FuzzUnmarshalDeviceCapability -fuzztime=$(FUZZTIME) ./internal/northbound/scheduler/
	go test -fuzz=FuzzUnmarshalTime             -fuzztime=$(FUZZTIME) ./internal/northbound/scheduler/
	go test -fuzz=FuzzUnmarshalDERControlList   -fuzztime=$(FUZZTIME) ./internal/northbound/scheduler/
	go test -fuzz=FuzzBusDecode                 -fuzztime=$(FUZZTIME) ./internal/bus/
