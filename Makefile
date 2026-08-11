# Portable across two setups:
#   - macOS host: `make vm` creates a nested-virt Linux VM (Lima) and every
#     other target runs inside it. This is the develop-on-a-laptop path;
#     nested virtualization adds scheduling jitter (see docs/measurements.md).
#   - Linux host with /dev/kvm (e.g. a bare-metal cloud box): run the targets
#     directly — no Lima, no nesting, clean numbers. `make vm` is a no-op.
#
# One-time:  make vm setup up   then  make demo / make bench
REPO := $(CURDIR)
UNAME := $(shell uname -s)
GOARCH := $(shell go env GOARCH)
GUEST ?= 172.30.0.50

ifeq ($(UNAME),Darwin)
  RUN     := limactl shell --workdir $(REPO) fcmig --
  COMPOSE := $(RUN) docker compose -f $(REPO)/deploy/docker-compose.yml
else
  RUN     :=
  COMPOSE := docker compose -f $(REPO)/deploy/docker-compose.yml
endif

.PHONY: vm binaries setup up down demo watch bench hostile plot test clean

## vm: (macOS only) create the nested-virt Linux VM; no-op on Linux
vm:
ifeq ($(UNAME),Darwin)
	limactl start --name=fcmig lima/fcmig.yaml --tty=false
else
	@echo "Running on Linux directly; no VM needed (ensure /dev/kvm exists)."
endif

## binaries: cross-compile the Go control plane, guest workload and prober
binaries:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o build/migrated ./cmd/migrated
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o build/guestd   ./cmd/guestd
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o build/fcprobe  ./cmd/fcprobe

## setup: build Firecracker from source (+ patches), fetch kernel, build rootfs
setup: binaries
	$(RUN) $(REPO)/scripts/setup-host.sh

## up: start host-a, host-b and the client container
up:
	mkdir -p bench-results
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

## demo: boot vm0 on host-a, then live-migrate it to host-b under probing
demo: up
	$(RUN) $(REPO)/scripts/run-demo.sh

## watch: follow the guest's own view of itself — a counter that must keep
## climbing and an integrity check that must stay at zero. Run this in a second
## terminal during `make demo`: it is the guest saying, from inside, that the
## migration never interrupted it. Ctrl-C to stop.
watch:
	$(COMPOSE) exec client sh -c \
	  'while true; do curl -s http://$(GUEST):7780/status; echo; sleep 0.5; done'

## bench: N ping-pong migrations, blackout distribution (bench-results/)
bench:
	$(COMPOSE) exec -T client /opt/fcmig/fcprobe \
	  -csv /bench-results/blackout.csv -n $(or $(N),50) bench

## plot: render bench-results/blackout.csv to a histogram (needs gnuplot)
plot:
	$(RUN) gnuplot -c $(REPO)/scripts/histogram.gnuplot \
	  $(REPO)/bench-results/blackout.csv $(REPO)/bench-results/histogram.png

## hostile: crank the guest's dirty rate, then migrate (auto-converge demo)
hostile:
	$(COMPOSE) exec -T client curl -fsS -X POST "http://172.30.0.50:7780/dirty?mbps=$(or $(MBPS),400)"
	$(COMPOSE) exec -T client /opt/fcmig/fcprobe migrate

## test: vet + unit tests for our packages (not the vendored firecracker tree)
test:
	go vet ./cmd/... ./internal/...
	go test ./cmd/... ./internal/...

clean: down
	rm -rf build bench-results
