FROM golang:1.25 AS builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -a -o cloudflared-ingress-router ./cmd

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/cloudflared-ingress-router .
USER 65532:65532

ENTRYPOINT ["/cloudflared-ingress-router"]
