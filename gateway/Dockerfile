# ─── Stage 1: Build ──────────────────────────────────────────────────────────
FROM golang:1.21-alpine AS builder

# Install git (diperlukan oleh beberapa go module)
RUN apk add --no-cache git

WORKDIR /build

# Copy dependency files dulu (cache layer)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -o wa-gateway -ldflags="-s -w" .

# ─── Stage 2: Runtime ────────────────────────────────────────────────────────
FROM alpine:3.19

# ca-certificates diperlukan untuk HTTPS (download gambar dari firebase storage)
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# Copy binary dari stage build
COPY --from=builder /build/wa-gateway .

# Buat folder data (untuk SQLite session WA)
RUN mkdir -p /app/data

# Port yang di-expose
EXPOSE 3000

# Jalankan gateway
CMD ["./wa-gateway"]
