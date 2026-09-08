// Package secret keeps provider credentials on disk, encrypted with a local
// key. Credentials never travel back through MCP responses: tools report only
// whether a field is set, never its value.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ErrNotFound is returned when a provider has no stored credentials.
var ErrNotFound = errors.New("secret: no credentials stored")

// keyEnv holds a base64 encoded 32 byte key. When unset, a key file is created
// next to the credential file with 0600 permissions.
const keyEnv = "RECEIPTS_SECRET_KEY"

// Store is a small encrypted key/value store, safe for concurrent use.
type Store struct {
	path    string
	keyPath string

	mu   sync.RWMutex
	key  []byte
	data map[string]map[string]string
}

// Open loads (or creates) the store under dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secret: create state dir: %w", err)
	}
	s := &Store{
		path:    filepath.Join(dir, "credentials.enc"),
		keyPath: filepath.Join(dir, "secret.key"),
		data:    map[string]map[string]string{},
	}
	key, err := s.loadKey()
	if err != nil {
		return nil, err
	}
	s.key = key
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Get returns a copy of the fields stored for a provider.
func (s *Store) Get(provider string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fields, ok := s.data[provider]
	if !ok {
		return nil, ErrNotFound
	}
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	return out, nil
}

// Field returns a single stored field, or "" when it is not set.
func (s *Store) Field(provider, name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[provider][name]
}

// Set replaces every field of a provider and persists the store.
func (s *Store) Set(provider string, fields map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make(map[string]string, len(fields))
	for k, v := range fields {
		if v == "" {
			continue
		}
		copied[k] = v
	}
	s.data[provider] = copied
	return s.saveLocked()
}

// Merge updates the given fields of a provider, leaving the others untouched.
// An empty value deletes the field.
func (s *Store) Merge(provider string, fields map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.data[provider]
	if !ok {
		current = map[string]string{}
	}
	for k, v := range fields {
		if v == "" {
			delete(current, k)
			continue
		}
		current[k] = v
	}
	s.data[provider] = current
	return s.saveLocked()
}

// Delete drops every credential of a provider.
func (s *Store) Delete(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, provider)
	return s.saveLocked()
}

// Providers lists the providers that have credentials stored.
func (s *Store) Providers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data))
	for name := range s.data {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// FieldNames lists which fields a provider has stored, for status reporting.
// The values are never returned.
func (s *Store) FieldNames(provider string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data[provider]))
	for name := range s.data[provider] {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Path is where the encrypted credentials live, for status reporting.
func (s *Store) Path() string { return s.path }

func (s *Store) loadKey() ([]byte, error) {
	if encoded := os.Getenv(keyEnv); encoded != "" {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("secret: %s is not valid base64: %w", keyEnv, err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("secret: %s must decode to 32 bytes, got %d", keyEnv, len(key))
		}
		return key, nil
	}

	switch raw, err := os.ReadFile(s.keyPath); {
	case err == nil:
		key, decodeErr := base64.StdEncoding.DecodeString(string(raw))
		if decodeErr != nil || len(key) != 32 {
			return nil, fmt.Errorf("secret: key file %s is corrupt; delete it and log in again", s.keyPath)
		}
		return key, nil
	case errors.Is(err, fs.ErrNotExist):
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		encoded := base64.StdEncoding.EncodeToString(key)
		if err := os.WriteFile(s.keyPath, []byte(encoded), 0o600); err != nil {
			return nil, fmt.Errorf("secret: write key file: %w", err)
		}
		return key, nil
	default:
		return nil, err
	}
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	plain, err := s.decrypt(raw)
	if err != nil {
		return fmt.Errorf("secret: cannot decrypt %s (wrong key?): %w", s.path, err)
	}
	return json.Unmarshal(plain, &s.data)
}

func (s *Store) saveLocked() error {
	plain, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	sealed, err := s.encrypt(plain)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) encrypt(plain []byte) ([]byte, error) {
	gcm, err := s.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func (s *Store) decrypt(sealed []byte) ([]byte, error) {
	gcm, err := s.gcm()
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, body := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	return gcm.Open(nil, nonce, body, nil)
}

func (s *Store) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
