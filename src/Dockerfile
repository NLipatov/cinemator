# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY . .
ARG VERSION=dev-build
RUN go build -ldflags="-X cinemator/internal/buildinfo.Version=${VERSION}" -o cinemator ./main.go

# Final stage: minimal image with ffmpeg
FROM alpine:latest
RUN apk add --no-cache ffmpeg
WORKDIR /app
COPY --from=builder /app/cinemator /app/cinemator
COPY presentation/web/client/index /app/presentation/web/client/index
COPY presentation/web/client/static /app/presentation/web/client/static
EXPOSE 8000
ENTRYPOINT ["./cinemator"]
