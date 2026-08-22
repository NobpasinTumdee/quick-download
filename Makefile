# Convenience targets for Linux/macOS. Windows users: use build.ps1.

BIN := bin/quick-download

.PHONY: all build test extension clean run fmt

all: test build extension

build:
	cd backend && go build -trimpath -ldflags '-s -w' -o ../$(BIN) .

test:
	cd backend && go test ./...

fmt:
	cd backend && gofmt -w . && go vet ./...

extension:
	cd extension && npm install && npm run build

run: build
	./$(BIN) --daemon

clean:
	rm -rf bin extension/dist
