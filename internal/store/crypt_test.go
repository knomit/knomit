package store

import (
	"testing"
)

func TestCrypt_RoundTrip(t *testing.T) {
	c, err := NewCrypt([]byte("test-key-material-for-knomit"))
	if err != nil {
		t.Fatalf("NewCrypt: %v", err)
	}

	original := "ghp_abc123_my_secret_token"
	encrypted, err := c.Encrypt(original)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if encrypted == original {
		t.Error("encrypted should differ from plaintext")
	}
	if encrypted == "" {
		t.Error("encrypted should not be empty")
	}

	decrypted, err := c.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if decrypted != original {
		t.Errorf("Decrypt = %q, want %q", decrypted, original)
	}
}

func TestCrypt_EmptyString(t *testing.T) {
	c, err := NewCrypt([]byte("test-key"))
	if err != nil {
		t.Fatalf("NewCrypt: %v", err)
	}

	enc, err := c.Encrypt("")
	if err != nil {
		t.Fatalf("Encrypt empty: %v", err)
	}
	if enc != "" {
		t.Errorf("Encrypt('') = %q, want empty", enc)
	}

	dec, err := c.Decrypt("")
	if err != nil {
		t.Fatalf("Decrypt empty: %v", err)
	}
	if dec != "" {
		t.Errorf("Decrypt('') = %q, want empty", dec)
	}
}

func TestCrypt_DifferentCiphertexts(t *testing.T) {
	c, err := NewCrypt([]byte("test-key"))
	if err != nil {
		t.Fatalf("NewCrypt: %v", err)
	}

	// Same plaintext should produce different ciphertexts (random nonce).
	enc1, _ := c.Encrypt("secret")
	enc2, _ := c.Encrypt("secret")
	if enc1 == enc2 {
		t.Error("two encryptions of same plaintext should differ (random nonce)")
	}
}

func TestCrypt_WrongKey(t *testing.T) {
	c1, _ := NewCrypt([]byte("key-one"))
	c2, _ := NewCrypt([]byte("key-two"))

	encrypted, _ := c1.Encrypt("secret")
	_, err := c2.Decrypt(encrypted)
	if err == nil {
		t.Error("decrypting with wrong key should fail")
	}
}

func TestCrypt_InvalidCiphertext(t *testing.T) {
	c, _ := NewCrypt([]byte("key"))

	_, err := c.Decrypt("not-base64!!!")
	if err == nil {
		t.Error("invalid base64 should fail")
	}

	_, err = c.Decrypt("dG9vc2hvcnQ=") // valid base64 but too short
	if err == nil {
		t.Error("short ciphertext should fail")
	}
}
