# Build stage
FROM golang:alpine AS builder
WORKDIR /app

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /app/irrigator .

# Runtime stage
FROM alpine:latest
RUN apk add --no-cache git ca-certificates
WORKDIR /app

COPY --from=builder /app/irrigator /usr/local/bin/irrigator

ENTRYPOINT ["irrigator", "mcp"]
