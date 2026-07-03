.PHONY: all build-amd64 build-squeezebox clean run setup-go1.14 generate

all: build-amd64 build-squeezebox

# Generate assets.go by scanning the plugins/ directory
generate:
	@echo "Generating embedded asset mapping in assets.go..."
	python3 generate_assets.py

# Build statically linked binary for x86_64 Linux (using host system compiler)
build-amd64: generate
	@echo "Building for x86_64 Linux..."
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o server_amd64 .

# Download and set up Go 1.14.15 (the last Go compiler fully supporting Linux kernel versions below 2.6.32)
setup-go1.14:
	@if [ ! -d "go1.14" ]; then \
		echo "Downloading Go 1.14.15 for legacy kernel compatibility (Linux 2.6.26)..."; \
		curl -sSL -o go1.14.tar.gz https://dl.google.com/go/go1.14.15.linux-amd64.tar.gz || wget -q -O go1.14.tar.gz https://dl.google.com/go/go1.14.15.linux-amd64.tar.gz; \
		mkdir -p go1.14; \
		tar -C go1.14 -xzf go1.14.tar.gz; \
		rm go1.14.tar.gz; \
	fi

# Build statically linked binary for Squeezebox Radio (ARMv5te, using Go 1.14 for kernel 2.6.26 compatibility)
build-squeezebox: setup-go1.14 generate
	@echo "Building for Squeezebox Radio (ARMv5, Linux kernel 2.6.26) using Go 1.14..."
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=5 ./go1.14/go/bin/go build -ldflags="-s -w" -o server_arm5 .

# Run the local server
run: build-amd64
	./server_amd64

# Clean built binaries and legacy compiler
clean:
	rm -f server server_amd64 server_arm5 assets.go
	rm -rf go1.14
