package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
)

type Track struct {
	ID        int64     `json:"id"`
	Owner     string    `json:"-"`
	VideoID   string    `json:"video_id"`
	Title     string    `json:"title"`
	Artist    string    `json:"artist"`
	Status    Status    `json:"status"`
	Log       string    `json:"log"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (t *Track) Sanitise() error {
	t.Artist = santise(t.Artist)
	t.Title = santise(t.Title)
	t.Owner = ownerSantise(t.Owner)

	if len(t.VideoID) != 11 {
		return fmt.Errorf("video id is not the correct length, was %d, should be 11", len(t.VideoID))
	}

	_, err := base64.RawURLEncoding.DecodeString(t.VideoID)
	if err != nil {
		return fmt.Errorf("video id was not base64")
	}

	return nil
}

// ─────────────────────────────────────────────
// Server
// ─────────────────────────────────────────────

type Server struct {
	db     *sql.DB
	mu     sync.Mutex
	outDir string
}

func NewServer(db *sql.DB, outDir string) *Server {
	return &Server{db: db, outDir: outDir}
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

func (s *Server) insertTrack(t *Track) (int64, error) {
	if err := t.Sanitise(); err != nil {
		return 0, err
	}

	var id int64
	err := s.db.QueryRow(
		`INSERT INTO tracks (video_id, title, artist, status, log, owner, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(video_id) DO UPDATE SET
             status     = excluded.status,
             log        = excluded.log,
             owner      = excluded.owner,
             updated_at = excluded.updated_at
         	WHERE tracks.status = 'failed'
		 RETURNING id`,
		t.VideoID, t.Title, t.Artist, t.Status, t.Log, t.Owner,
		t.CreatedAt.Format(time.RFC3339Nano), t.UpdatedAt.Format(time.RFC3339Nano)).Scan(&id)

	switch {
	case err == nil:
		// fresh insert OR the conditional update fired — either way we got the id back
		return id, nil
	case errors.Is(err, sql.ErrNoRows):
		// row exists but status wasn't 'failed', so neither insert nor update happened
		err = s.db.QueryRow(`SELECT id FROM tracks WHERE video_id = ?`, t.VideoID).Scan(&id)
		return id, err
	default:
		return 0, err
	}
}

func (s *Server) updateStatus(id int64, status Status, logMsg string) {
	s.db.Exec(
		`UPDATE tracks SET status = ?, log = ?, updated_at = ? WHERE id = ?`,
		status, logMsg, time.Now().UTC(), id)
}

// fetch a single failed track to reinsert into the download queue
func (s *Server) pluckFailed() (track *Track, err error) {
	row := s.db.QueryRow(
		`UPDATE tracks SET status = 'queued', updated_at = ?
		 WHERE id = (
		     SELECT id FROM tracks WHERE status = 'failed' ORDER BY id LIMIT 1
		 )
		 RETURNING id, video_id, title, artist, status, log, created_at, updated_at`,
		time.Now().UTC().Format(time.RFC3339Nano))

	return scanTrack(row)
}

func (s *Server) listTracks(from int64) ([]Track, error) {
	rows, err := s.db.Query(
		`SELECT id, video_id, title, artist, status, log, created_at, updated_at
		 FROM tracks WHERE (id < $1 OR $1 = 0) ORDER BY id DESC LIMIT 60 `, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tracks := make([]Track, 0, 60)
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			continue
		}
		tracks = append(tracks, *t)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return tracks, nil
}

type Stats struct {
	Done        int
	Pending     int
	Downloading int
	Failed      int
}

func (s *Server) countracks() (Stats, error) {
	rows, err := s.db.Query(
		`SELECT
			COUNT(CASE WHEN status = 'done' THEN 1 END) AS done_count,
			COUNT(CASE WHEN status = 'pending' THEN 1 END) AS pending_count, 
			COUNT(CASE WHEN status = 'failed' THEN 1 END) AS failed_count,
			COUNT(CASE WHEN status = 'downloading' THEN 1 END) AS downloading_count
		FROM tracks;`)
	if err != nil {
		return Stats{}, err
	}

	defer rows.Close()

	var stat Stats
	err = rows.Scan(
		&stat.Done, &stat.Pending, &stat.Failed, &stat.Downloading)
	if err != nil {
		return stat, err
	}

	return stat, nil
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

func (s *Server) download(t *Track) {
	if t.Status != StatusQueued && t.Status != StatusFailed {
		return
	}

	s.updateStatus(t.ID, StatusDownloading, "starting download...")

	if err := t.Sanitise(); err != nil {
		s.updateStatus(t.ID, StatusFailed, "failed to santise track")
		return
	}

	cleanedPath := filepath.Join("/", filepath.Clean(fmt.Sprintf("%s - %s [%%(id)s].%%(ext)s", t.Title, t.Artist)))

	outTemplate := filepath.Join(s.outDir, time.Now().Format(time.DateOnly), cleanedPath)

	u, err := url.Parse("https://music.youtube.com/watch")
	if err != nil {
		log.Println("failed to parse url: ", err)
		t.Status = StatusFailed
		t.Log = err.Error()
		return
	}

	q := u.Query()
	q.Set("v", t.VideoID)

	u.RawQuery = q.Encode()

	var out []byte

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), Config.DownloadTimeout)

		arguments := []string{
			"--no-playlist",
			"--embed-thumbnail",
			"--add-metadata",
			"-x", "--audio-format", "opus",
			"-f", "bestaudio",
			"--postprocessor-args", fmt.Sprintf("ffmpeg:-metadata owner=%s", ownerSantise(t.Owner)),
			"-o", outTemplate,
			u.String(),
		}

		cmd := exec.CommandContext(ctx, "/usr/local/bin/yt-dlp", arguments...)

		out, err = cmd.CombinedOutput()
		cancel()

		if err == nil {
			break
		}

		time.Sleep(20 * time.Second)
		log.Printf("[FAIL %d/5] %s: %v:\n%s", i, t.VideoID, err, out)
	}

	logStr := strings.TrimSpace(string(out))

	if err != nil {
		s.updateStatus(t.ID, StatusFailed, logStr)
		log.Printf("[FAIL] %s: %v:\n%s", t.VideoID, err, out)
		return
	}

	s.updateStatus(t.ID, StatusDone, logStr)
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
	t := &Track{
		VideoID:   body.VideoID,
		Title:     body.Title,
		Artist:    body.Artist,
		Status:    StatusQueued,
		Log:       "",
		CreatedAt: now,
		UpdatedAt: now,
		Owner:     owner,
	}

	id, err := s.insertTrack(t)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		log.Println("failed to insert track: ", err)
		return
	}
	t.ID = id

	go s.download(t)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{"status": "queued", "trackId": id})
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)

	go func() {
		for {
			s.mu.Lock()
			failedTrack, err := s.pluckFailed()
			s.mu.Unlock()

			if err != nil {
				// eventually this hits no rows and we leave this
				return
			}

			log.Println("retrying download of failed track: ", failedTrack.Title, failedTrack.VideoID)

			s.download(failedTrack)

			time.Sleep(5 * time.Minute)
		}

	}()
}

// GET /api/tracks - return the last 60 tracks
func (s *Server) handleTracks(w http.ResponseWriter, r *http.Request) {

	var (
		from int64
		err  error
	)
	if offset := r.URL.Query().Get("from"); offset != "" {
		from, err = strconv.ParseInt(offset, 10, 64)
		if err != nil {
			log.Println("invalid from offset: ", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	tracks, err := s.listTracks(from)
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

// GET /api/stats
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.countracks()
	if err != nil {
		log.Println("failed to count tracks: ", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
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

	reg.Name = ownerSantise(reg.Name)

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

	go updateYtdlp()

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
	mux.HandleFunc("GET /api/stats", srv.stats)
	mux.HandleFunc("POST /api/retry", srv.retry)

	mux.HandleFunc("POST /api/register", srv.handleRegister)
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

func updateYtdlp() {
	for {
		exec.Command("/usr/local/bin/yt-dlp", "-U")
		time.Sleep(24 * time.Hour)
	}
}
