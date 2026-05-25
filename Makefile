# Build, cross-compile, and install targets for mvs.
#
# Usage:
#   make             build for the host (binary at ./mvs)
#   make install     copy ./mvs to $(PREFIX)/bin (default ~/.local/bin)
#   make release     cross-compile to dist/ for all supported platforms
#   make clean

VERSION ?= 0.1.0
PREFIX  ?= $(HOME)/.local
LDFLAGS := -s -w -X main.version=$(VERSION)

PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64

.PHONY: build install release clean tidy

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o mvs .

tidy:
	go mod tidy

install: build
	install -d $(PREFIX)/bin
	install -m 0755 mvs $(PREFIX)/bin/mvs
	@echo "installed: $(PREFIX)/bin/mvs"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; \
	*) echo "warning: $(PREFIX)/bin is not on \$$PATH";; esac

release:
	@rm -rf dist && mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out="dist/mvs-$(VERSION)-$$os-$$arch$$ext"; \
		echo "→ $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -trimpath -ldflags '$(LDFLAGS)' -o $$out . || exit 1; \
	done
	@echo
	@echo "release artifacts in ./dist/"
	@ls -lh dist/

clean:
	rm -f mvs
	rm -rf dist
