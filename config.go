package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"
)

type config struct {
	Addr            string `json:"addr,omitempty"`
	ExternalAddress string `json:"external_address"`

	DownloadsPath string `json:"downloads_path,omitempty"`
	DBPath        string `json:"db_path,omitempty"`

	Debug bool `json:"debug,omitempty"`

	DownloadTimeout time.Duration `json:"download_timeout,omitempty"`

	Key string
}

var Config config

func Load(path string) error {

	config, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	dec := json.NewDecoder(bytes.NewBuffer(config))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&Config); err != nil {
		return fmt.Errorf("failed to decode config file: %w", err)
	}

	if Config.Addr == "" {
		Config.Addr = "localhost:8080"
	}

	if Config.DownloadsPath == "" {
		Config.DownloadsPath = "downloads"
	}

	if Config.DBPath == "" {
		Config.DBPath = "ytdl.db"
	}

	if Config.DownloadTimeout == 0 {
		Config.DownloadTimeout = 5 * time.Minute
	}

	if Config.ExternalAddress == "" {
		Config.ExternalAddress = "http://" + Config.Addr
	}

	if Config.Key == "" || len(Config.Key) < 16 {
		if len(Config.Key) < 16 {
			log.Println("The set admin password for registration is too short <16 chars")
		}

		buff := make([]byte, 16)
		_, err := rand.Read(buff)
		if err != nil {
			return err
		}

		Config.Key = hex.EncodeToString(buff)
		log.Println("Temporary admin key: ", Config.Key)
	}

	return err
}
