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
FROM debian:trixie-slim

# yt-dlp needs Python, ffmpeg (for audio extraction + muxing),
# and AtomicParsley (for embedding thumbnails into m4a/mp4).
# AtomicParsley is the default thumbnail embedder yt-dlp reaches for;
# without it --embed-thumbnail silently skips on non-mkv containers.
RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 \
        python3-pip \
        python3-mutagen \ 
        ffmpeg \
        atomicparsley \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*


WORKDIR /app

COPY --from=builder /app/ytdl-server ./ytdl-server
COPY entry.sh .
RUN chmod +x entry.sh && chmod 777 /usr/local/bin

# Sensible defaults — all overridable via config.json or env vars
ENV CONFIG_PATH=/app/config.json

VOLUME ["/downloads", "/data"]

ENTRYPOINT ["/app/entry.sh"]