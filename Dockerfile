# Build stage — uses pre-built binary from the host (faster than in-container Go build)
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY bin/huawei-elb-controller-linux-amd64 /huawei-elb-controller
USER nonroot:nonroot
ENTRYPOINT ["/huawei-elb-controller"]
