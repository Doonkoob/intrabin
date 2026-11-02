package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SecretBox encrypts small secrets at rest with AES-256-GCM.
// Ciphertext format: version(1) || nonce(12) || ciphertext+tag.
type SecretBox struct {
	aead cipher.AEAD
}

const secretBoxVersion = 1

// NewSecretBox creates a SecretBox from a 32-byte key.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead}, nil
}

// Seal encrypts plaintext; aad binds the ciphertext to a context (e.g. record id).
func (s *SecretBox) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+s.aead.Overhead())
	out = append(out, secretBoxVersion)
	out = append(out, nonce...)
	return s.aead.Seal(out, nonce, plaintext, aad), nil
}

// Open decrypts ciphertext produced by Seal.
func (s *SecretBox) Open(ciphertext, aad []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(ciphertext) < 1+ns+s.aead.Overhead() || ciphertext[0] != secretBoxVersion {
		return nil, errors.New("invalid ciphertext")
	}
	pt, err := s.aead.Open(nil, ciphertext[1:1+ns], ciphertext[1+ns:], aad)
	if err != nil {
		return nil, errors.New("decryption failed (wrong master key or corrupted data)")
	}
	return pt, nil
}

// LoadOrCreateMasterKey returns the master key from env (hex or base64, 32 bytes)
// or from <dataDir>/master.key, generating the file (0600) on first start.
func LoadOrCreateMasterKey(envValue, dataDir string) ([]byte, bool, error) {
	if envValue != "" {
		k, err := decodeKey(envValue)
		if err != nil {
			return nil, false, fmt.Errorf("INTRABIN_MASTER_KEY: %w", err)
		}
		return k, false, nil
	}
	path := filepath.Join(dataDir, "master.key")
	if b, err := os.ReadFile(path); err == nil {
		k, err := decodeKey(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		return k, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return nil, false, err
	}
	return key, true, nil
}

func decodeKey(v string) ([]byte, error) {
	if b, err := hex.DecodeString(v); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(v); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errors.New("must be 32 bytes encoded as hex (64 chars) or base64")
}
