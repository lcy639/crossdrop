package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const chunkSize int64 = 8 << 20
const maxFiles = 100
const maxRooms = 100

type config struct {
	dataDir, webDir     string
	ttl                 time.Duration
	maxFile, maxStorage int64
}

func loadConfig() (config, error) {
	c := config{dataDir: os.Getenv("DATA_DIR"), webDir: os.Getenv("WEB_DIR"), ttl: 24 * time.Hour, maxFile: 10 << 30, maxStorage: 50 << 30}
	if c.dataDir == "" {
		c.dataDir = "./data"
	}
	if c.webDir == "" {
		c.webDir = "/app/web"
	}
	for key, target := range map[string]*int64{"MAX_FILE_BYTES": &c.maxFile, "MAX_STORAGE_BYTES": &c.maxStorage} {
		if raw := os.Getenv(key); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n <= 0 || n > 1<<50 {
				return c, fmt.Errorf("invalid %s", key)
			}
			*target = n
		}
	}
	if raw := os.Getenv("ROOM_TTL_HOURS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 168 {
			return c, errors.New("ROOM_TTL_HOURS must be 1..168")
		}
		c.ttl = time.Duration(n) * time.Hour
	}
	return c, nil
}

type FileRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Ready     bool   `json:"ready"`
	Verifying bool   `json:"-"`
	Error     string `json:"error,omitempty"`
}
type room struct {
	mu           sync.Mutex
	ID           string        `json:"id"`
	Code         string        `json:"code"`
	OwnerHash    string        `json:"ownerHash"`
	ReceiveToken string        `json:"receiveToken"`
	Expires      time.Time     `json:"expires"`
	Files        []*FileRecord `json:"files"`
}
type rateBucket struct {
	start time.Time
	count int
}
type app struct {
	c                 config
	mu                sync.Mutex
	rooms             map[string]*room
	reserved          int64
	rateMu            sync.Mutex
	rates             map[string]rateBucket
	verificationSlots chan struct{}
}

func randomHex(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func hashString(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}
func validID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && len(id) == 32
}
func (a *app) roomDir(r *room) string                { return filepath.Join(a.c.dataDir, r.ID) }
func (a *app) fileDir(r *room, f *FileRecord) string { return filepath.Join(a.roomDir(r), f.ID) }
func (a *app) chunkPath(r *room, f *FileRecord, index int) string {
	return filepath.Join(a.fileDir(r, f), fmt.Sprintf("%06d.chunk", index))
}
func chunkCount(f *FileRecord) int { return int((f.Size + chunkSize - 1) / chunkSize) }
func expectedSize(f *FileRecord, index int) int64 {
	remaining := f.Size - int64(index)*chunkSize
	if remaining < chunkSize {
		return remaining
	}
	return chunkSize
}
func atomicJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".metadata-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(b); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
func (a *app) save(r *room) error { return atomicJSON(filepath.Join(a.roomDir(r), "room.json"), r) }
func newApp(c config) (*app, error) {
	if err := os.MkdirAll(c.dataDir, 0700); err != nil {
		return nil, err
	}
	a := &app{c: c, rooms: make(map[string]*room), rates: make(map[string]rateBucket), verificationSlots: make(chan struct{}, 2)}
	entries, err := os.ReadDir(c.dataDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(c.dataDir, entry.Name(), "room.json"))
		if os.IsNotExist(err) {
			// A crash before the first metadata commit cannot publish a room.
			if err := os.RemoveAll(filepath.Join(c.dataDir, entry.Name())); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read room %s: %w", entry.Name(), err)
		}
		var r room
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		if r.ID != entry.Name() || len(r.Code) != 6 || len(r.OwnerHash) != 64 || len(r.ReceiveToken) != 64 {
			return nil, errors.New("invalid stored room")
		}
		for _, f := range r.Files {
			if !validID(f.ID) || f.Size < 0 || f.Size > 1<<50 {
				return nil, errors.New("invalid stored file")
			}
			a.reserved += f.Size
			// Incomplete atomic writes are never advertised as uploaded chunks.
			files, _ := os.ReadDir(a.fileDir(&r, f))
			for _, file := range files {
				if !file.IsDir() && (strings.HasPrefix(file.Name(), ".") || (f.Ready && strings.HasSuffix(file.Name(), ".chunk"))) {
					os.Remove(filepath.Join(a.fileDir(&r, f), file.Name()))
				}
			}
		}
		a.rooms[r.ID] = &r
	}
	a.cleanup()
	return a, nil
}
func (a *app) allow(key string, limit int) bool {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	now := time.Now()
	for k, b := range a.rates {
		if now.Sub(b.start) > time.Minute {
			delete(a.rates, k)
		}
	}
	b := a.rates[key]
	if b.start.IsZero() {
		b.start = now
	}
	if b.count >= limit {
		return false
	}
	if len(a.rates) >= 4096 && b.count == 0 {
		return false
	}
	b.count++
	a.rates[key] = b
	return true
}
func (a *app) cleanup() {
	a.mu.Lock()
	rooms := make([]*room, 0, len(a.rooms))
	for _, r := range a.rooms {
		rooms = append(rooms, r)
	}
	a.mu.Unlock()
	for _, r := range rooms {
		r.mu.Lock()
		verifying := false
		for _, f := range r.Files {
			if f.Verifying {
				verifying = true
			}
		}
		if time.Now().After(r.Expires) && !verifying {
			if err := os.RemoveAll(a.roomDir(r)); err != nil {
				log.Printf("cleanup room %s: %v", r.ID, err)
			} else {
				a.mu.Lock()
				if a.rooms[r.ID] == r {
					delete(a.rooms, r.ID)
					for _, f := range r.Files {
						a.reserved -= f.Size
					}
				}
				a.mu.Unlock()
			}
		}
		r.mu.Unlock()
	}
}
func (a *app) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanup()
		}
	}
}
