// Package secrets stores Radarr/Sonarr/Jellyfin connection info (URL +
// API key) encrypted at rest, separate from the human-edited coldarr.yaml.
// It also resolves the env-var override layer: a container env var like
// RADARR_URL/RADARR_API_KEY always wins over whatever is stored, so a
// deployment can be configured entirely via Docker env vars, entirely via
// the web GUI, or a mix of both per app.
//
// Threat model: this defends against a connection's URL/API key leaking
// through casual exposure of the config directory (pasted into a support
// thread, an accidentally-committed volume backup, etc.) - not against an
// attacker who already has read access to the container/filesystem, since
// the decryption key lives right next to the ciphertext on the same
// volume.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	SourceEnv    = "env"
	SourceStored = "stored"
	SourceNone   = "none"
)

// Connection is one app's connection info. Enabled is only meaningful for
// apps that can be toggled off independent of having credentials on file
// (currently just Jellyfin) - Radarr/Sonarr are considered enabled
// whenever both URL and APIKey are set.
type Connection struct {
	URL     string `json:"url"`
	APIKey  string `json:"api_key"`
	Enabled bool   `json:"enabled"`
}

func (c Connection) configured() bool {
	return c.URL != "" && c.APIKey != ""
}

type encryptedBlob struct {
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type Store struct {
	keyPath  string
	dataPath string
	key      []byte

	mu    sync.RWMutex
	conns map[string]Connection
}

// LoadOrCreate loads the encrypted connection store from dir, generating a
// new random encryption key on first run if one doesn't already exist.
func LoadOrCreate(dir string) (*Store, error) {
	keyPath := filepath.Join(dir, ".coldarr.key")
	dataPath := filepath.Join(dir, "connections.enc.json")

	key, err := loadOrGenerateKey(keyPath)
	if err != nil {
		return nil, err
	}

	s := &Store{keyPath: keyPath, dataPath: dataPath, key: key, conns: map[string]Connection{}}

	raw, err := os.ReadFile(dataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dataPath, err)
	}
	if len(raw) == 0 {
		return s, nil
	}

	var blobs map[string]encryptedBlob
	if err := json.Unmarshal(raw, &blobs); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", dataPath, err)
	}

	for app, blob := range blobs {
		conn, err := decrypt(key, blob)
		if err != nil {
			return nil, fmt.Errorf("decrypting stored connection for %q: %w (was %s replaced or lost?)", app, err, keyPath)
		}
		s.conns[app] = conn
	}

	return s, nil
}

func loadOrGenerateKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		if len(raw) != 32 {
			return nil, fmt.Errorf("encryption key at %s is not 32 bytes - it looks corrupt", path)
		}
		return raw, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading encryption key %s: %w", path, err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating encryption key: %w", err)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("writing encryption key %s: %w", path, err)
	}
	return key, nil
}

func encrypt(key []byte, conn Connection) (encryptedBlob, error) {
	plaintext, err := json.Marshal(conn)
	if err != nil {
		return encryptedBlob{}, err
	}

	gcm, err := newGCM(key)
	if err != nil {
		return encryptedBlob{}, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return encryptedBlob{}, fmt.Errorf("generating nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	return encryptedBlob{Nonce: nonce, Ciphertext: ciphertext}, nil
}

func decrypt(key []byte, blob encryptedBlob) (Connection, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return Connection{}, err
	}

	plaintext, err := gcm.Open(nil, blob.Nonce, blob.Ciphertext, nil)
	if err != nil {
		return Connection{}, fmt.Errorf("decrypting: %w", err)
	}

	var conn Connection
	if err := json.Unmarshal(plaintext, &conn); err != nil {
		return Connection{}, err
	}
	return conn, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initializing cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// Get returns the raw stored connection for app (ignoring env overrides),
// for rendering the "what's saved" state in the GUI's edit form.
func (s *Store) Get(app string) (Connection, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.conns[app]
	return c, ok
}

// Set stores (encrypted) a connection for app, persisting immediately.
func (s *Store) Set(app string, conn Connection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[app] = conn
	return s.persistLocked()
}

// Delete removes any stored connection for app.
func (s *Store) Delete(app string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, app)
	return s.persistLocked()
}

func (s *Store) persistLocked() error {
	blobs := make(map[string]encryptedBlob, len(s.conns))
	for app, conn := range s.conns {
		blob, err := encrypt(s.key, conn)
		if err != nil {
			return fmt.Errorf("encrypting connection for %q: %w", app, err)
		}
		blobs[app] = blob
	}

	data, err := json.MarshalIndent(blobs, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding connection store: %w", err)
	}

	tmp := s.dataPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.dataPath); err != nil {
		return fmt.Errorf("saving %s: %w", s.dataPath, err)
	}
	return nil
}

// Effective resolves the connection Coldarr should actually use for app:
// env vars named <APP>_URL / <APP>_API_KEY (and, for jellyfin,
// <APP>_ENABLED) always take precedence over whatever is stored. source
// tells the caller (the GUI) whether the effective value came from the
// environment, the encrypted store, or neither.
func (s *Store) Effective(app string) (conn Connection, source string) {
	stored, ok := s.Get(app)

	prefix := strings.ToUpper(app) + "_"
	envURL := os.Getenv(prefix + "URL")
	envKey := os.Getenv(prefix + "API_KEY")

	if envURL == "" && envKey == "" {
		if !ok || !stored.configured() {
			return Connection{}, SourceNone
		}
		if app != "jellyfin" {
			stored.Enabled = true
		}
		return stored, SourceStored
	}

	conn = stored
	if envURL != "" {
		conn.URL = envURL
	}
	if envKey != "" {
		conn.APIKey = envKey
	}
	conn.Enabled = true
	if v := os.Getenv(prefix + "ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			conn.Enabled = b
		}
	}
	return conn, SourceEnv
}
