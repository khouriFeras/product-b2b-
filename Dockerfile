# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Allow Go to use the toolchain version required by go.mod (e.g. 1.25.6)
ENV GOTOOLCHAIN=auto

# Copy dependency files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /productb2b .

# Runtime stage
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY --from=builder /productb2b .

EXPOSE 3000

# Pass env at runtime; optional: mount .env or .shopify_token
ENTRYPOINT ["./productb2b"]
