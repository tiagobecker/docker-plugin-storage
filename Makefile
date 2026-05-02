GOCACHE ?= $(CURDIR)/.gocache
GOMODCACHE ?= $(CURDIR)/.gomodcache
GO ?= go
PLUGIN_IMAGE ?= dps-plugin-rootfs:local
PLUGIN_NAME ?= dps:latest

.PHONY: build test clean plugin-rootfs plugin-create

build:
	mkdir -p bin
	env GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) build -trimpath -o bin/dpsd ./cmd/dpsd
	env GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) build -trimpath -o bin/dpsctl ./cmd/dpsctl

test:
	env GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test ./...

clean:
	rm -rf bin .gocache .gomodcache packaging/docker-plugin/rootfs

plugin-rootfs:
	docker build -f packaging/Dockerfile.plugin-rootfs -t $(PLUGIN_IMAGE) .
	mkdir -p packaging/docker-plugin/rootfs
	docker create --name dps-rootfs-tmp $(PLUGIN_IMAGE) true
	docker export dps-rootfs-tmp | tar -x -C packaging/docker-plugin/rootfs
	docker rm -f dps-rootfs-tmp

plugin-create: plugin-rootfs
	docker plugin create $(PLUGIN_NAME) packaging/docker-plugin
