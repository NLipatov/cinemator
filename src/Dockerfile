# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.27-alpine AS builder
ARG VERSION=dev-build
WORKDIR /app
COPY . .
RUN go build -ldflags "-X main.version=${VERSION}" -o cinemator ./main.go

# Final stage: minimal image with ffmpeg
FROM alpine:latest
RUN apk add --no-cache ffmpeg
WORKDIR /app
COPY --from=builder /app/cinemator /app/cinemator
COPY web/client/index /app/web/client/index
COPY web/client/static /app/web/client/static
EXPOSE 8000
ENTRYPOINT ["./cinemator"]
