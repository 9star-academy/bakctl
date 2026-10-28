package crypto

import (
	"bytes"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	passphrase := "my-super-strong-backup-password-1234"
	key := DeriveKey(passphrase)
	originalData := []byte("sqlite database backup content with secret tokens and user data")

	encrypted, err := Encrypt(originalData, key)
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}

	if bytes.Equal(encrypted, originalData) {
		t.Fatalf("encrypted data should not match plaintext")
	}

	decrypted, err := Decrypt(encrypted, key)
	if err != nil {
		t.Fatalf("decryption failed: %v", err)
	}

	if !bytes.Equal(decrypted, originalData) {
		t.Fatalf("decrypted data does not match original: got %s, expected %s", string(decrypted), string(originalData))
	}

	// Wrong password test
	wrongKey := DeriveKey("wrong-password")
	_, err = Decrypt(encrypted, wrongKey)
	if err == nil {
		t.Fatalf("expected decryption error with wrong password")
	}
}
