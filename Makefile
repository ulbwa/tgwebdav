APP := tgwebdav

# Static, portable release builds: CGO off so the binary has no libc dependency
# (runs on Alpine/musl, NAS firmware, weak ARM boards, and Termux on phones);
# -trimpath strips local filesystem paths; -s -w drop the symbol table and DWARF
# (~30% smaller). 32-bit ARM is built GOARM=6 so one binary covers armv6 + armv7
# (Pi Zero/1/2/3, most NAS, 32-bit phones).
export CGO_ENABLED := 0
GOFLAGS := -trimpath
LDFLAGS := -s -w

# windows: no usable 32-bit-ARM target (Windows RT is dead).
# macOS:   Go only supports darwin/amd64 and darwin/arm64 (no 32-bit / arm32).
PLATFORMS := \
	linux/amd64 \
	linux/386 \
	linux/arm64 \
	linux/arm \
	windows/amd64 \
	windows/386 \
	windows/arm64 \
	darwin/amd64 \
	darwin/arm64

.PHONY: build clean run checksums

build:
	@mkdir -p dist
	@$(foreach platform,$(PLATFORMS), \
		$(eval OS   := $(word 1,$(subst /, ,$(platform)))) \
		$(eval ARCH := $(word 2,$(subst /, ,$(platform)))) \
		$(eval EXT  := $(if $(filter windows,$(OS)),.exe,)) \
		$(eval ARM  := $(if $(filter arm,$(ARCH)),6,)) \
		$(eval NAME := $(APP)-$(OS)-$(ARCH)$(if $(ARM),v$(ARM),)$(EXT)) \
		echo "  building $(NAME)" && \
		GOOS=$(OS) GOARCH=$(ARCH) $(if $(ARM),GOARM=$(ARM),) \
			go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o dist/$(NAME) ./cmd/tgwebdav; \
	)
	@echo "done -> dist/"

# Optional: SHA-256 sums of everything in dist/ (run after `make build`).
checksums:
	@cd dist && shasum -a 256 * > SHA256SUMS && cat SHA256SUMS

clean:
	rm -rf dist/

run:
	go run ./cmd/tgwebdav server
