package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

// Optional app-level chunk-text encryption (#377). When an ENCRYPTION_KEY is
// configured, a chunk's text column is stored AES-256-GCM encrypted at rest;
// embeddings and content_hash are computed from plaintext in the core before the
// store sees the row, so vector search and dedup are unaffected. Off by default —
// chunk text is stored as plaintext, exactly as before (the recommended at-rest
// posture remains full-volume/disk encryption, #371; this is a defense-in-depth
// opt-in for a shared or managed Postgres).
//
// Caveat: the lexical/full-text path (SearchChunksLexical, #0012) indexes the
// stored column, so it cannot match *encrypted* chunks — retrieval degrades to
// vector-only for them. Dense/vector search (the primary path) is unaffected.

// chunkTextSentinel prefixes an app-encrypted chunk text so decryptText can tell
// ciphertext from a plaintext row and pass plaintext through unchanged — which is
// what makes encryption opt-in and backward-compatible: pre-encryption rows stay
// readable, and a mix of plaintext + ciphertext rows (key enabled later) both work.
const chunkTextSentinel = "$brainiac$aesgcm256$v1$"

var chunkAEAD cipher.AEAD // process-wide chunk-text cipher; nil = encryption off

// SetChunkCipher configures app-level chunk-text encryption from a 32-byte AES-256
// key (#377). A nil/empty key disables it (the default — plaintext, as before).
// Called once at boot.
func SetChunkCipher(key []byte) error {
	if len(key) == 0 {
		chunkAEAD = nil
		return nil
	}
	if len(key) != 32 {
		return fmt.Errorf("encryption key must be 32 bytes (AES-256), got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	chunkAEAD = aead
	return nil
}

// encryptText returns text ready to store: sentinel-prefixed ciphertext when a
// cipher is configured, else the plaintext unchanged.
func encryptText(text string) (string, error) {
	if chunkAEAD == nil {
		return text, nil
	}
	nonce := make([]byte, chunkAEAD.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := chunkAEAD.Seal(nonce, nonce, []byte(text), nil)
	return chunkTextSentinel + base64.StdEncoding.EncodeToString(ct), nil
}

// decryptInto decrypts a scanned text field in place — the read-side counterpart
// of encryptText, applied wherever a chunk's text is read back.
func decryptInto(p *string) error {
	v, err := decryptText(*p)
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// decryptText reverses encryptText. A value without the sentinel is a plaintext row
// and is returned unchanged (so pre-encryption data and a later-enabled key both
// work). Ciphertext with no configured key is an error rather than silent garbage.
func decryptText(stored string) (string, error) {
	if !strings.HasPrefix(stored, chunkTextSentinel) {
		return stored, nil
	}
	if chunkAEAD == nil {
		return "", fmt.Errorf("chunk text is encrypted but no ENCRYPTION_KEY is configured")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, chunkTextSentinel))
	if err != nil {
		return "", fmt.Errorf("decode encrypted chunk: %w", err)
	}
	ns := chunkAEAD.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("encrypted chunk too short")
	}
	pt, err := chunkAEAD.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt chunk: %w", err)
	}
	return string(pt), nil
}
