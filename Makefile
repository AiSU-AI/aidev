# Minimal Makefile for aidev. Most users should run ./install.sh instead.
# This file is for developers who prefer `make` targets.

.PHONY: build install test vet clean

BIN_DIR ?= $(HOME)/.local/bin
BIN     := $(BIN_DIR)/aidev

build:
	go build -o aidev ./cmd/aidev

install: build
	install -d $(BIN_DIR)
	install -m 0755 aidev $(BIN)
	$(BIN) install
	$(BIN) plugin install
	@echo ""
	@echo "aidev installed to $(BIN)"
	@echo "run: $(BIN) doctor"

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f aidev
