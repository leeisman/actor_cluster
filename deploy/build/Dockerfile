# Build Image (Multi-stage)
# 必須與 go.mod 的 `go 1.25.0` 一致；GOTOOLCHAIN=auto 讓較舊的 base 映像能自動下載正確 toolchain
FROM golang:1.25-alpine AS builder
ENV GOTOOLCHAIN=auto
RUN apk add --no-cache ca-certificates git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 靜態編譯供 alpine 執行，縮小 Volume
RUN CGO_ENABLED=0 GOOS=linux go build -o actor-node ./cmd/node
RUN CGO_ENABLED=0 GOOS=linux go build -o actor-client ./cmd/client

# Actor Node Runtime
FROM alpine:latest AS node
WORKDIR /app
COPY --from=builder /app/actor-node .
EXPOSE 50051
ENTRYPOINT ["./actor-node"]

# Client (Load Generator) Runtime
FROM alpine:latest AS client
WORKDIR /app
COPY --from=builder /app/actor-client .
ENTRYPOINT ["./actor-client"]
