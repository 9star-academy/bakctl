package crypto

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

const (
	streamMagic     = "BAKCTLENC2\n"
	streamChunkSize = 64 * 1024
	keyIterations   = 600000
)

// EncryptStream writes the v2 encrypted format with bounded memory. Each frame is
// authenticated along with the header, its length and its sequence number. An
// authenticated empty final frame makes truncation detectable, including at a
// frame boundary. A fresh salt and nonce prefix are generated for every file.
func EncryptStream(dst io.Writer, src io.Reader, passphrase string) error {
	header := make([]byte, len(streamMagic)+16+8)
	copy(header, streamMagic)
	if _, err := io.ReadFull(rand.Reader, header[len(streamMagic):]); err != nil {
		return err
	}
	aead, err := streamAEAD(passphrase, header)
	if err != nil {
		return err
	}
	if err := writeBytes(dst, header); err != nil {
		return err
	}
	buf := make([]byte, streamChunkSize)
	for sequence := uint64(0); ; sequence++ {
		if sequence > math.MaxUint32 {
			return fmt.Errorf("encrypted stream is too large")
		}
		n, readErr := io.ReadFull(src, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		if n > 0 {
			if err := writeFrame(dst, aead, header, uint32(sequence), buf[:n]); err != nil {
				return err
			}
			if readErr == io.ErrUnexpectedEOF {
				sequence++
				if sequence > math.MaxUint32 {
					return fmt.Errorf("encrypted stream is too large")
				}
				return writeFrame(dst, aead, header, uint32(sequence), nil)
			}
		} else {
			return writeFrame(dst, aead, header, uint32(sequence), nil)
		}
	}
}

// DecryptStream authenticates v2 frames, and retains read compatibility with v1
// nonce+ciphertext files. Callers must stage output until this function succeeds:
// an error may follow previously authenticated frames. Legacy v1 decoding still
// needs memory proportional to the legacy ciphertext's size.
func DecryptStream(dst io.Writer, src io.Reader, passphrase string) error {
	reader := bufio.NewReader(src)
	magic, err := reader.Peek(len(streamMagic))
	if err != nil && err != io.EOF {
		return err
	}
	if !bytes.Equal(magic, []byte(streamMagic)) {
		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		plaintext, err := Decrypt(data, DeriveKey(passphrase))
		if err != nil {
			return err
		}
		return writeBytes(dst, plaintext)
	}
	header := make([]byte, len(streamMagic)+16+8)
	if _, err := io.ReadFull(reader, header); err != nil {
		return fmt.Errorf("truncated encryption header: %w", err)
	}
	aead, err := streamAEAD(passphrase, header)
	if err != nil {
		return err
	}
	for sequence := uint64(0); ; sequence++ {
		if sequence > math.MaxUint32 {
			return fmt.Errorf("encrypted stream is too large")
		}
		var length [4]byte
		if _, err := io.ReadFull(reader, length[:]); err != nil {
			return fmt.Errorf("missing encrypted frame or final marker: %w", err)
		}
		n := binary.BigEndian.Uint32(length[:])
		if n > streamChunkSize {
			return fmt.Errorf("invalid encrypted frame length %d", n)
		}
		ciphertext := make([]byte, int(n)+aead.Overhead())
		if _, err := io.ReadFull(reader, ciphertext); err != nil {
			return fmt.Errorf("truncated encrypted frame: %w", err)
		}
		nonce, aad := frameParameters(header, uint32(sequence), n)
		plaintext, err := aead.Open(ciphertext[:0], nonce, ciphertext, aad)
		if err != nil {
			return fmt.Errorf("authentication failed (wrong passphrase or damaged backup): %w", err)
		}
		if n == 0 {
			if _, err := reader.ReadByte(); err != io.EOF {
				if err != nil {
					return err
				}
				return fmt.Errorf("unexpected data after final encrypted frame")
			}
			return nil
		}
		if err := writeBytes(dst, plaintext); err != nil {
			return err
		}
	}
}

func streamAEAD(passphrase string, header []byte) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, header[len(streamMagic):len(streamMagic)+16], keyIterations, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func frameParameters(header []byte, sequence, length uint32) ([]byte, []byte) {
	nonce := make([]byte, 12)
	copy(nonce, header[len(streamMagic)+16:])
	binary.BigEndian.PutUint32(nonce[8:], sequence)
	aad := make([]byte, len(header)+8)
	copy(aad, header)
	binary.BigEndian.PutUint32(aad[len(header):], sequence)
	binary.BigEndian.PutUint32(aad[len(header)+4:], length)
	return nonce, aad
}

func writeFrame(dst io.Writer, aead cipher.AEAD, header []byte, sequence uint32, plaintext []byte) error {
	nonce, aad := frameParameters(header, sequence, uint32(len(plaintext)))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(plaintext)))
	if err := writeBytes(dst, length[:]); err != nil {
		return err
	}
	return writeBytes(dst, aead.Seal(nil, nonce, plaintext, aad))
}

func writeBytes(dst io.Writer, data []byte) error {
	n, err := dst.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
