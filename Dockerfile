FROM --platform=$BUILDPLATFORM golang:alpine AS builder
ARG TARGETARCH TARGETOS
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o tailscale-exporter main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/tailscale-exporter .
EXPOSE 8080
ENTRYPOINT ["./tailscale-exporter"]
