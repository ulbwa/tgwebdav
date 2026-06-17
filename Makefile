APP    := tgwebdav
MODULE := $(shell go list -m)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X $(MODULE)/internal/version.Version=$(VERSION)"

PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	linux/arm \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64 \
	windows/arm64 \
	freebsd/amd64

.PHONY: build clean run

build:
	$(foreach platform,$(PLATFORMS), \
		$(eval OS   := $(word 1,$(subst /, ,$(platform)))) \
		$(eval ARCH := $(word 2,$(subst /, ,$(platform)))) \
		$(eval EXT  := $(if $(filter windows,$(OS)),.exe,)) \
		GOOS=$(OS) GOARCH=$(ARCH) go build $(LDFLAGS) \
			-o dist/$(APP)-$(OS)-$(ARCH)$(EXT) \
			./cmd/tgwebdav; \
	)

clean:
	rm -rf dist/

run:
	go run ./cmd/tgwebdav server
