# Build stage — compiles the controller binary from source
FROM golang:1.26 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /huawei-elb-controller ./cmd/

# Runtime stage — minimal image with CA certificates
FROM gcr.io/distroless/base:nonroot
WORKDIR /
COPY --from=builder /huawei-elb-controller /huawei-elb-controller
USER nonroot:nonroot
ENTRYPOINT ["/huawei-elb-controller"]
