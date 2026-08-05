package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Crypt provides AES-256-GCM encryption for credential storage.
// The encryption key is derived from the SSH private key via HKDF.
type Crypt struct {
	gcm cipher.AEAD
}

// NewCrypt derives an AES-256 key from the given key material (typically
// the raw bytes of the SSH private key file) and returns a Crypt instance.
func NewCrypt(keyMaterial []byte) (*Crypt, error) {
	// Derive 32-byte AES key using HKDF-SHA256.
	h := hkdf.New(sha256.New, keyMaterial, nil, []byte("knomit-credential-encryption"))
	aesKey := make([]byte, 32)
	if _, err := io.ReadFull(h, aesKey); err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}

	return &Crypt{gcm: gcm}, nil
}

// encrypt encrypts plaintext and returns a base64-encoded ciphertext.
// Returns empty string for empty input.
func (c *Crypt) encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := c.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decrypt decodes a base64-encoded ciphertext and returns the plaintext.
// Returns empty string for empty input.
func (c *Crypt) decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}

	nonceSize := c.gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := c.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plaintext), nil
}

// Encrypt is the exported form of encrypt, for callers outside this package
// that persist credentials (the repo registry in internal/repos).
func (c *Crypt) Encrypt(plaintext string) (string, error) { return c.encrypt(plaintext) }

// Decrypt is the exported form of decrypt. See Encrypt.
func (c *Crypt) Decrypt(encoded string) (string, error) { return c.decrypt(encoded) }
