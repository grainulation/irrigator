# Build stage
FROM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS builder
WORKDIR /app

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY internal/ ./internal/
COPY examples/embed.go examples/terminal-swarm.json ./examples/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /app/irrigator .

# Runtime stage
FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
RUN apk add --no-cache git ca-certificates
RUN adduser -D -u 10001 irrigator
WORKDIR /app

COPY --from=builder /app/irrigator /usr/local/bin/irrigator

USER irrigator
ENTRYPOINT ["irrigator", "mcp"]
