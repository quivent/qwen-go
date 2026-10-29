.PHONY: build test install
build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o qwen-go .
test:
	go test -race ./...
install: build
	install -m 755 qwen-go $(HOME)/.local/bin/qwen-go
