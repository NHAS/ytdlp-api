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
FROM debian:sid-slim

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
        npm \
        yt-dlp \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deno.land/install.sh -o install.sh && \
    chmod +x install.sh && \
    DENO_INSTALL="/usr" ./install.sh -y && \
    rm install.sh 

WORKDIR /app

COPY --from=builder /app/ytdl-server ./ytdl-server

# Sensible defaults — all overridable via config.json or env vars
ENV CONFIG_PATH=/app/config.json

VOLUME ["/downloads", "/data"]

ENTRYPOINT ["./ytdl-server"]