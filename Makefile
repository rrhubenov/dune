GO ?= go
PREFIX ?= /usr/local

.PHONY: build test roottest install clean

build:
	$(GO) build -o bin/duned ./cmd/duned
	$(GO) build -o bin/dune ./cmd/dune
	GOOS=darwin GOARCH=arm64 $(GO) build -o bin/dune-darwin-arm64 ./cmd/dune

test:
	$(GO) vet ./...
	$(GO) test ./...

# Root integration tests need Firecracker and a reflink fs; they build here
# and run with sudo (see each test for its env vars).
roottest:
	$(GO) test -c -o bin/netns.test ./internal/netns
	$(GO) test -c -o bin/fc.test ./internal/backend/firecracker
	$(GO) test -c -o bin/vm.test ./internal/vm
	sudo DUNE_TEST_ROOT=1 bin/netns.test -test.v
	sudo DUNE_TEST_FC=1 bin/fc.test -test.v
	sudo DUNE_TEST_FC=1 bin/vm.test -test.v

install: build
	sudo install -m 755 bin/duned $(DESTDIR)$(PREFIX)/bin/duned
	sudo install -m 755 bin/dune $(DESTDIR)$(PREFIX)/bin/dune
	sudo install -m 644 contrib/duned.service /etc/systemd/system/duned.service
	sudo systemctl daemon-reload

clean:
	rm -rf bin
