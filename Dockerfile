# ─────────────────────────────────────────────
# Stage 1: Build the Go binary
# ─────────────────────────────────────────────
FROM golang:1.26 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux \
    go build -ldflags="-s -w" -o ytdl-server .


# ─────────────────────────────────────────────
# Stage 2: Runtime
# ─────────────────────────────────────────────
FROM debian:bookworm-slim

# yt-dlp needs Python, ffmpeg (for audio extraction + muxing),
# and AtomicParsley (for embedding thumbnails into m4a/mp4).
# AtomicParsley is the default thumbnail embedder yt-dlp reaches for;
# without it --embed-thumbnail silently skips on non-mkv containers.
RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 \
        ffmpeg \
        atomicparsley \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

# Install yt-dlp from the official release binary rather than
# the distro package so we always get a recent version.
RUN curl -fsSL \
        "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp" \
        -o /usr/local/bin/yt-dlp \
    && chmod +x /usr/local/bin/yt-dlp

# go-sqlite3 is a CGo package, so the binary is dynamically linked.
# Pull in the C runtime libraries it needs.
RUN apt-get update && apt-get install -y --no-install-recommends && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /app/ytdl-server ./ytdl-server

# Sensible defaults — all overridable via config.json or env vars
ENV CONFIG_PATH=/app/config.json

VOLUME ["/downloads", "/data"]

ENTRYPOINT ["./ytdl-server"]