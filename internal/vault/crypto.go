// Package secrets owns the credential vault: encryption primitives, the
// leak-resistant Secret type, the in-process atomic.Pointer cache, and the
// rotation state machine. Everything that touches plaintext lives here.
//
// Crypto choices (locked in DECISIONS.md, mirroring Dockyard):
//
//	AES-256-GCM, PBKDF2-SHA256 600,000 iterations, 32-byte salt per secret,
//	12-byte nonce per encryption, version byte prefix for forward compat.
//
// Stored format:
//
//	[version(1)] [salt(32)] [nonce(12)] [ciphertext(...)]
//
// The version byte enables transparent re-encryption on read after a master-key
// rotation: future v2 blobs co-exist with v1 blobs and migrate lazily.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/pbkdf2"
)

const (
	// VersionV1 is the format byte for the v1 blob layout.
	VersionV1 byte = 0x01

	saltLen     = 32
	nonceLen    = 12
	keyLen      = 32 // AES-256
	pbkdf2Iters = 600_000
)

// ErrInvalidBlob is returned when a blob is malformed (too short or
// has an unknown version byte).
var ErrInvalidBlob = errors.New("secrets: invalid encrypted blob")

// ErrInvalidMasterKey is returned when the master key length is wrong.
var ErrInvalidMasterKey = errors.New("secrets: master key must be 32 bytes")

// Encrypt encrypts plaintext with a unique salt + nonce and returns
// the v1 blob layout: [version][salt][nonce][ciphertext].
func Encrypt(masterKey, plaintext []byte) ([]byte, error) {
	if len(masterKey) != keyLen {
		return nil, ErrInvalidMasterKey
	}

	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("secrets: salt: %w", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}

	dek := deriveKey(masterKey, salt)
	defer zero(dek)

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)

	out := make([]byte, 0, 1+saltLen+nonceLen+len(ct))
	out = append(out, VersionV1)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Decrypt reverses Encrypt. Returns ErrInvalidBlob if the version byte
// or layout is wrong, or any underlying GCM error if the key is wrong
// or the data was tampered with.
func Decrypt(masterKey, blob []byte) ([]byte, error) {
	if len(masterKey) != keyLen {
		return nil, ErrInvalidMasterKey
	}
	if len(blob) < 1+saltLen+nonceLen {
		return nil, ErrInvalidBlob
	}
	switch blob[0] {
	case VersionV1:
		// fallthrough
	default:
		return nil, fmt.Errorf("%w: unsupported version 0x%02x", ErrInvalidBlob, blob[0])
	}
	salt := blob[1 : 1+saltLen]
	nonce := blob[1+saltLen : 1+saltLen+nonceLen]
	ct := blob[1+saltLen+nonceLen:]

	dek := deriveKey(masterKey, salt)
	defer zero(dek)

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}
	return gcm.Open(nil, nonce, ct, nil)
}

// deriveKey runs PBKDF2-HMAC-SHA256 with the locked-in iteration count.
// Each call allocates a fresh derived-key slice the caller is responsible
// for zeroing.
func deriveKey(masterKey, salt []byte) []byte {
	return pbkdf2.Key(masterKey, salt, pbkdf2Iters, keyLen, sha256.New)
}

// zero overwrites the slice contents. Best-effort: Go's GC may have
// copied the data elsewhere already, but doing this for derived keys
// shrinks the residency window noticeably under pressure.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
