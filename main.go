package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed index.html
var index []byte

// ─────────────────────────────────────────────
// Models
// ─────────────────────────────────────────────

type Status string

const (
	StatusQueued      Status = "queued"
	StatusDownloading Status = "downloading"
	StatusDone        Status = "done"
	StatusFailed      Status = "failed"
	StatusSkipped     Status = "skipped"
)

type Track struct {
	ID        int64     `json:"id"`
	VideoID   string    `json:"video_id"`
	Title     string    `json:"title"`
	Artist    string    `json:"artist"`
	Status    Status    `json:"status"`
	Log       string    `json:"log"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ─────────────────────────────────────────────
// SSE broker
// ─────────────────────────────────────────────

type Broker struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func NewBroker() *Broker { return &Broker{clients: make(map[chan []byte]struct{})} }

func (b *Broker) Subscribe() chan []byte {
	ch := make(chan []byte, 32)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *Broker) Publish(v any) {
	data, _ := json.Marshal(v)
	msg := append([]byte("data: "), data...)
	msg = append(msg, '\n', '\n')
	b.mu.Lock()
	for ch := range b.clients {
		select {
		case ch <- msg:
		default:
		}
	}
	b.mu.Unlock()
}

// ─────────────────────────────────────────────
// Server
// ─────────────────────────────────────────────

type Server struct {
	db     *sql.DB
	broker *Broker
	mu     sync.Mutex
	outDir string
}

func NewServer(db *sql.DB, outDir string) *Server {
	return &Server{db: db, broker: NewBroker(), outDir: outDir}
}

// ─────────────────────────────────────────────
// DB helpers
// ─────────────────────────────────────────────

func (s *Server) findByVideoID(videoID string) (*Track, error) {
	row := s.db.QueryRow(
		`SELECT id, video_id, title, artist, status, log, created_at, updated_at
		 FROM tracks WHERE video_id = ?`, videoID)
	return scanTrack(row)
}

func (s *Server) insertTrack(t *Track, owner string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO tracks (video_id, title, artist, status, log, owner, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.VideoID, t.Title, t.Artist, t.Status, t.Log, owner,
		t.CreatedAt.Format(time.RFC3339Nano), t.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Server) updateStatus(id int64, status Status, logMsg string) {
	s.db.Exec(
		`UPDATE tracks SET status = ?, log = ?, updated_at = ? WHERE id = ?`,
		status, logMsg, time.Now().UTC(), id)
}

func (s *Server) listTracks() ([]Track, error) {
	rows, err := s.db.Query(
		`SELECT id, video_id, title, artist, status, log, created_at, updated_at
		 FROM tracks ORDER BY created_at DESC LIMIT 60`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tracks []Track
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			continue
		}
		tracks = append(tracks, *t)
	}
	return tracks, nil
}

type scanner interface {
	Scan(...any) error
}

func scanTrack(s scanner) (*Track, error) {
	var t Track
	var createdAt, updatedAt string
	err := s.Scan(&t.ID, &t.VideoID, &t.Title, &t.Artist,
		&t.Status, &t.Log, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return &t, nil
}

// ─────────────────────────────────────────────
// Download worker
// ─────────────────────────────────────────────

func (s *Server) download(t Track) {
	s.updateStatus(t.ID, StatusDownloading, "starting download...")
	t.Status = StatusDownloading
	s.broker.Publish(t)

	cleanedPath := filepath.Join("/", filepath.Clean(fmt.Sprintf("%s - %s [%%(id)s].%%(ext)s", t.Title, t.Artist)))

	outTemplate := filepath.Join(s.outDir, time.Now().Format(time.DateOnly), cleanedPath)

	u, err := url.Parse("https://music.youtube.com")
	if err != nil {
		log.Println("failed to parse url: ", err)
		t.Status = StatusFailed
		t.Log = err.Error()
		return
	}

	q := u.Query()
	q.Set("v", t.VideoID)

	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), Config.DownloadTimeout)
	cmd := exec.CommandContext(ctx, "yt-dlp",
		"--no-playlist",
		"--embed-thumbnail",
		"--add-metadata",
		"-x", "--audio-format", "opus",
		"-f", "bestaudio",
		"-o", outTemplate,
		u.String(),
	)
	out, err := cmd.CombinedOutput()
	cancel()

	logStr := strings.TrimSpace(string(out))

	if err != nil {
		s.updateStatus(t.ID, StatusFailed, logStr)
		t.Status = StatusFailed
		t.Log = logStr
		s.broker.Publish(t)
		log.Printf("[FAIL] %s: %v:\n%s", t.VideoID, err, out)
		return
	}

	s.updateStatus(t.ID, StatusDone, logStr)
	t.Status = StatusDone
	t.Log = logStr
	s.broker.Publish(t)
	log.Printf("[DONE] %s", t.Title)
}

// ─────────────────────────────────────────────
// HTTP handlers
// ─────────────────────────────────────────────

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if owner == "" {
		w.WriteHeader(http.StatusUnauthorized)
		log.Println("no owner specified")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// POST /download — called by the browser extension
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	owner := r.PathValue("owner")
	if owner == "" {
		w.WriteHeader(http.StatusUnauthorized)
		log.Println("no owner specified")
		return
	}

	var body struct {
		Title   string `json:"title"`
		Artist  string `json:"artist"`
		VideoID string `json:"videoId"`
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&body); err != nil {
		log.Println("failed to decode json: ", err)
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	if body.VideoID == "" {
		log.Println("VideoID required")

		http.Error(w, "VideoID required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Dedup: already downloaded or in flight?
	if existing, err := s.findByVideoID(body.VideoID); err == nil {
		if existing.Status == StatusDone {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status":  "skipped",
				"reason":  "already downloaded",
				"trackId": existing.ID,
			})
			return
		}
		if existing.Status == StatusDownloading || existing.Status == StatusQueued {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status":  "skipped",
				"reason":  "already in queue",
				"trackId": existing.ID,
			})
			return
		}
	}

	now := time.Now().UTC()
	t := Track{
		VideoID:   body.VideoID,
		Title:     body.Title,
		Artist:    body.Artist,
		Status:    StatusQueued,
		Log:       "",
		CreatedAt: now,
		UpdatedAt: now,
	}

	id, err := s.insertTrack(&t, owner)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		log.Println("failed to insert track: ", err)
		return
	}
	t.ID = id
	s.broker.Publish(t)

	go s.download(t)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{"status": "queued", "trackId": id})
}

// GET /api/tracks — full list
func (s *Server) handleTracks(w http.ResponseWriter, r *http.Request) {
	tracks, err := s.listTracks()
	if err != nil {
		log.Println("failed to list tracks: ", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if tracks == nil {
		tracks = []Track{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tracks)
}

// POST /api/register — set the registration information and generate api key
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {

	type registration struct {
		Name string
		Key  string
	}

	var reg registration

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&reg); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Println("client sent invalid registration request")
		return
	}

	if subtle.ConstantTimeCompare([]byte(reg.Key), []byte(Config.Key)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		log.Println("Client sent a request with an invalid admin key")
		return
	}

	buff := make([]byte, 16)
	_, err := rand.Read(buff)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	key := hex.EncodeToString(buff)

	now := time.Now()
	sum := sha256.Sum256([]byte(key))

	_, err = s.db.Exec(`INSERT INTO keys (key, owner, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		fmt.Sprintf("%x", sum),
		reg.Name,
		now.Format(time.RFC3339Nano),
		now.Format(time.RFC3339Nano))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println("failed to register new user: ", err)
		return
	}

	log.Println("Registered new user: ", reg.Name)

	w.Header().Set("Content-Type", "application/json")

	u, err := url.Parse("ext+ytdl://register")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println("failed to parse url: ", err)
		return
	}

	q := u.Query()
	q.Set("url", Config.ExternalAddress)
	q.Set("key", key)

	u.RawQuery = q.Encode()

	json.NewEncoder(w).Encode(struct {
		Url string `json:"url"`
	}{
		Url: u.String(),
	})
}

// GET /events — SSE stream
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.broker.Subscribe()
	defer s.broker.Unsubscribe(ch)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Do initial connection to make the connection be present as "alive"
	fmt.Fprintf(w, ": keepalive\n\n")
	flusher.Flush()

	// Send a keepalive comment every 15 s
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			w.Write(msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// GET / — embedded web UI
func handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !Config.Debug {
		w.Write(index)
		return
	}

	out, err := os.ReadFile("index.html")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%v", err)
		return
	}

	w.Write(out)

}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func (s *Server) isValidKey(rawKey string) (owner string, err error) {
	h := sha256.Sum256([]byte(rawKey))
	hex := fmt.Sprintf("%x", h)

	row := s.db.QueryRow(`SELECT owner FROM keys WHERE key = ?`, hex)
	if row.Err() != nil {
		return "", row.Err()
	}

	if err := row.Scan(&owner); err != nil {
		return "", err
	}

	return
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Authorisation")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		key := r.Header.Get("X-Authorisation")
		if key == "" {
			log.Println("No authorisation header specified")
			http.Error(w, "missing API key", http.StatusUnauthorized)
			return
		}

		owner, err := s.isValidKey(key)
		if err != nil {
			log.Println("Invalid api key: ", err)

			http.Error(w, "invalid API key", http.StatusUnauthorized)
			return
		}

		r.SetPathValue("owner", owner)

		next(w, r)
	}
}

// ─────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────

func mustMigrateDB(db *sql.DB) {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tracks (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			video_id   TEXT NOT NULL UNIQUE,
			owner      TEXT NOT NULL,
			title      TEXT,
			artist     TEXT,
			status     TEXT NOT NULL DEFAULT 'queued',
			log        TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_status ON tracks(status);
	`); err != nil {
		log.Fatal("Failed to create tracks table: ", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS keys (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			key        TEXT NOT NULL UNIQUE,   -- SHA-256 hex of the raw key
			owner      TEXT NOT NULL UNIQUE,   -- User that this key belongs to
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_key ON keys(key);
	`); err != nil {
		log.Fatal("Failed to create api keys table: ", err)
	}
}

func main() {

	configPath := getEnv("CONFIG_PATH", "/data/config.json")
	if err := Load(configPath); err != nil {
		log.Fatalf("failed to load configuration file %q: %v", configPath, err)
	}

	log.Printf("Loaded configuration: %#v", Config)

	if err := os.MkdirAll(Config.DownloadsPath, 0750); err != nil {
		log.Fatal("Failed to create directory:", err)
	}

	db, err := sql.Open("sqlite3", Config.DBPath+"?_journal_mode=WAL")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	mustMigrateDB(db)

	srv := NewServer(db, Config.DownloadsPath)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleUI)

	mux.HandleFunc("OPTIONS /download", srv.requireAuth(srv.handleDownload))
	mux.HandleFunc("POST /download", srv.requireAuth(srv.handleDownload))

	mux.HandleFunc("OPTIONS /check", srv.requireAuth(srv.handleCheck))
	mux.HandleFunc("POST /check", srv.requireAuth(srv.handleCheck))

	mux.HandleFunc("GET /api/tracks", srv.handleTracks)
	mux.HandleFunc("POST /api/register", srv.handleRegister)
	mux.HandleFunc("/events", srv.handleSSE)
	server := &http.Server{
		Addr:         Config.Addr,
		Handler:      http.MaxBytesHandler(mux, 1024*1024),
		ReadTimeout:  5 * time.Second,   // Max time to read the request
		WriteTimeout: 10 * time.Second,  // Max time to write the response
		IdleTimeout:  120 * time.Second, // Max time for keep-alive connections
	}

	log.Printf("ytdl-server listening on %s  |  Downloads path → %q", Config.Addr, Config.DownloadsPath)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}
