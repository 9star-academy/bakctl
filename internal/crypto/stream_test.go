package crypto

import (
	"bytes"
	"io"
	"testing"
)

func TestStreamEncryptionRoundTripAndLegacy(t *testing.T) {
	for _, size := range []int{0, 1, 65536, 65537, 160000} {
		data := bytes.Repeat([]byte{0x5a}, size)
		var encrypted, decrypted bytes.Buffer
		if err := EncryptStream(&encrypted, bytes.NewReader(data), "test passphrase"); err != nil {
			t.Fatal(err)
		}
		if err := DecryptStream(&decrypted, bytes.NewReader(encrypted.Bytes()), "test passphrase"); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, decrypted.Bytes()) {
			t.Fatalf("size %d round trip mismatch", size)
		}
	}
	legacy, err := Encrypt([]byte("legacy"), DeriveKey("password"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded bytes.Buffer
	if err := DecryptStream(&decoded, bytes.NewReader(legacy), "password"); err != nil {
		t.Fatal(err)
	}
	if decoded.String() != "legacy" {
		t.Fatalf("legacy=%q", decoded.String())
	}
}

func TestStreamRejectsWrongKeyTruncationCorruptionAndTrailingData(t *testing.T) {
	var b bytes.Buffer
	if err := EncryptStream(&b, bytes.NewReader(bytes.Repeat([]byte("x"), 70000)), "correct"); err != nil {
		t.Fatal(err)
	}
	ciphertext := b.Bytes()
	cases := []struct {
		name, password string
		data           []byte
	}{
		{"wrong password", "wrong", ciphertext},
		{"header truncated", "correct", ciphertext[:20]},
		{"payload truncated", "correct", ciphertext[:len(ciphertext)-1]},
		{"missing final marker", "correct", ciphertext[:len(ciphertext)-20]},
		{"trailing data", "correct", append(append([]byte(nil), ciphertext...), 1)},
	}
	for _, offset := range []int{len(streamMagic), len(streamMagic) + 16} {
		mutated := append([]byte(nil), ciphertext...)
		mutated[offset] ^= 0x80
		cases = append(cases, struct {
			name, password string
			data           []byte
		}{"header authentication", "correct", mutated})
	}
	corrupt := append([]byte(nil), ciphertext...)
	corrupt[len(corrupt)/2] ^= 0x80
	cases = append(cases, struct {
		name, password string
		data           []byte
	}{"corruption", "correct", corrupt})
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if err := DecryptStream(io.Discard, bytes.NewReader(tt.data), tt.password); err == nil {
				t.Fatal("expected authentication/format error")
			}
		})
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

type failingWriter struct{ remaining int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, io.ErrClosedPipe
	}
	if len(p) > w.remaining {
		n := w.remaining
		w.remaining = 0
		return n, io.ErrClosedPipe
	}
	w.remaining -= len(p)
	return len(p), nil
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestStreamPropagatesIOFailures(t *testing.T) {
	for _, limit := range []int{0, 36, 42} {
		if err := EncryptStream(&failingWriter{remaining: limit}, bytes.NewReader([]byte("content")), "key"); err == nil {
			t.Fatalf("writer limit=%d should fail", limit)
		}
	}
	if err := EncryptStream(io.Discard, errorReader{}, "key"); err == nil {
		t.Fatal("source read failure ignored")
	}
	if err := EncryptStream(shortWriter{}, bytes.NewReader(nil), "key"); err != io.ErrShortWrite {
		t.Fatalf("short write=%v", err)
	}
	if err := DecryptStream(io.Discard, errorReader{}, "key"); err == nil {
		t.Fatal("ciphertext read failure ignored")
	}
	var encrypted bytes.Buffer
	if err := EncryptStream(&encrypted, bytes.NewReader([]byte("content")), "key"); err != nil {
		t.Fatal(err)
	}
	if err := DecryptStream(&failingWriter{}, bytes.NewReader(encrypted.Bytes()), "key"); err == nil {
		t.Fatal("plaintext write failure ignored")
	}
	badLength := append([]byte(nil), encrypted.Bytes()...)
	start := len(streamMagic) + 16 + 8
	copy(badLength[start:start+4], []byte{0xff, 0xff, 0xff, 0xff})
	if err := DecryptStream(io.Discard, bytes.NewReader(badLength), "key"); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if err := DecryptStream(io.Discard, bytes.NewReader([]byte("short")), "key"); err == nil {
		t.Fatal("invalid legacy cipher accepted")
	}
}

func TestStreamRejectsFrameReordering(t *testing.T) {
	var encrypted bytes.Buffer
	if err := EncryptStream(&encrypted, bytes.NewReader(bytes.Repeat([]byte("abc"), 50000)), "key"); err != nil {
		t.Fatal(err)
	}
	data := append([]byte(nil), encrypted.Bytes()...)
	start := len(streamMagic) + 16 + 8
	frameSize := 4 + streamChunkSize + 16
	first := append([]byte(nil), data[start:start+frameSize]...)
	copy(data[start:start+frameSize], data[start+frameSize:start+2*frameSize])
	copy(data[start+frameSize:start+2*frameSize], first)
	if err := DecryptStream(io.Discard, bytes.NewReader(data), "key"); err == nil {
		t.Fatal("reordered authenticated frames accepted")
	}
}

func TestLegacyRejectsInvalidKeysAndShortCiphertext(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := Encrypt([]byte("x"), make([]byte, size)); err == nil {
			t.Fatalf("encrypt key length %d accepted", size)
		}
		if _, err := Decrypt([]byte("x"), make([]byte, size)); err == nil {
			t.Fatalf("decrypt key length %d accepted", size)
		}
	}
	if _, err := Decrypt([]byte("x"), make([]byte, 32)); err == nil {
		t.Fatal("short cipher accepted")
	}
}
