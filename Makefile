.PHONY: build run docker-build deploy tidy clean

# Build the controller binary
build:
	go build -o bin/huawei-elb-controller ./cmd

# Run locally against the current kubeconfig
run:
	go run ./cmd

# Build Docker image
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
