export GOTOOLCHAIN := local

# Version from the nearest git tag (e.g. v1.2.0, or v1.2.0-3-gabc1234 for later
# commits); the GitHub repository from the "origin" remote, for update checks.
VERSION ?= $(shell git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev)
REPO    ?= $(shell git config --get remote.origin.url 2>/dev/null | grep -oE 'github\.com[:/][^/]+/[^/]+' | sed -E 's#github\.com[:/]##; s#\.git$$##')
LDFLAGS := -s -w -X ytguard.Version=$(VERSION) -X ytguard.Repo=$(REPO)

.PHONY: build dist test vet install upgrade clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o ytguard ./cmd/ytguard
	@./ytguard version -v

# Every release binary in dist/: Linux for kid PCs, plus the console for
# Windows and macOS.
dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/ytguard-linux-amd64 ./cmd/ytguard
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/ytguard-linux-arm64 ./cmd/ytguard
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/ytguard-windows-amd64.exe ./cmd/ytguard
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/ytguard-macos-amd64 ./cmd/ytguard
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/ytguard-macos-arm64 ./cmd/ytguard
	cd dist && sha256sum ytguard-* > SHA256SUMS

test:
	go vet ./...
	go test ./...

vet:
	go vet ./...

# First install on this PC.
install: build
	sudo ./ytguard install

# Install this local build over an existing install.
upgrade: build
	sudo ./ytguard upgrade --file ./ytguard

clean:
	rm -f ytguard
	rm -rf dist
