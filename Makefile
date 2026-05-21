.PHONY: build run test release clean

build:
	go build -o bin/cc-router ./cmd/ccr

run:
	go run ./cmd/ccr --config config.toml

test:
	go test ./...

release:
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/cc-router-linux-amd64 ./cmd/ccr
	GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o bin/cc-router-windows-amd64.exe ./cmd/ccr

clean:
	rm -rf bin/
