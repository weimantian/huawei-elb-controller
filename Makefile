.PHONY: build build-linux run docker-build deploy tidy clean verify

# Build the controller binary for current platform
build:
	go build -o bin/huawei-elb-controller ./cmd

# Cross-compile for Linux/amd64 (for direct deployment without Docker)
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/huawei-elb-controller-linux-amd64 ./cmd

# Run locally against the current kubeconfig
run:
	go run ./cmd

# Build Docker image (multi-stage build compiles from source)
docker-build:
	docker build -t huawei-elb-controller:latest .

# Deploy to cluster (apply all manifests in deploy/)
deploy:
	kubectl apply -f deploy/

# Tidy and verify dependencies
tidy:
	go mod tidy

# Clean build artifacts
clean:
	rm -rf bin/

# Verify compilation
verify: tidy
	go build ./...
	go vet ./...
