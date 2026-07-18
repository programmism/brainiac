package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// Optional app-level encryption of sensitive free-text at rest (#377, #399, #403).
// When an ENCRYPTION_KEY is configured, chunk text, edge rationale (why), and node
// summary/rollup are stored AES-256-GCM encrypted; embeddings and content_hash are
// computed from plaintext in the core before the store sees the row, so vector
// search and dedup are unaffected. Off by default — text is stored as plaintext,
// exactly as before (the recommended at-rest posture remains full-volume/disk
// encryption, #371; this is a defense-in-depth opt-in for a shared or managed
// Postgres).
//
// Key rotation (#403): multiple keys coexist. Each ciphertext is tagged with a key
// id (the sentinel below), writes use the PRIMARY key, and reads pick the key by id
// from a ring that holds the primary plus any RETIRED keys — so an operator can
// rotate ENCRYPTION_KEY while old data still decrypts, then run `kb reencrypt` to
// migrate everything onto the new key and drop the old one.
//
// Caveat: the lexical/full-text path (SearchChunksLexical, #0012) indexes the
// stored column, so it cannot match *encrypted* chunks — retrieval degrades to
// vector-only for them. Dense/vector search (the primary path) is unaffected.

// sentinelPrefix marks an app-encrypted value; what follows is "<keyid>$<base64>".
// Its presence (vs absence) is what makes encryption opt-in and backward-compatible:
// a plaintext row lacks it and passes through unchanged. The historical single-key
// format used the literal id "v1" (legacyKeyID) and is still read, mapped to the
// primary key.
const sentinelPrefix = "$brainiac$aesgcm256$"
const legacyKeyID = "v1"

// chunkTextSentinel is the encrypted-value marker (kept for tests/back-compat refs).
const chunkTextSentinel = sentinelPrefix

var (
	cipherPrimaryID string                 // key id used for new writes ("" = encryption off)
	cipherRing      map[string]cipher.AEAD // key id -> cipher, for reads (primary + retired)
)

// keyID is a short stable identifier for a key (first 8 hex of its SHA-256), so
// config just lists keys and their ids are derived — no manual id bookkeeping.
func keyID(key []byte) string {
	h := sha256.Sum256(key)
	return hex.EncodeToString(h[:4])
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes (AES-256), got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SetChunkCiphers configures app-level encryption from a primary key (used for new
// writes) plus any retired keys still accepted for reads during rotation (#403).
// An empty primary disables encryption (the default). Called once at boot.
func SetChunkCiphers(primary []byte, retired ...[]byte) error {
	if len(primary) == 0 {
		cipherPrimaryID, cipherRing = "", nil
		return nil
	}
	ring := map[string]cipher.AEAD{}
	pid := keyID(primary)
	pa, err := newAEAD(primary)
	if err != nil {
		return err
	}
	ring[pid] = pa
	for _, k := range retired {
		if len(k) == 0 {
			continue
		}
		a, err := newAEAD(k)
		if err != nil {
			return fmt.Errorf("retired key: %w", err)
		}
		ring[keyID(k)] = a
	}
	cipherPrimaryID, cipherRing = pid, ring
	return nil
}

// SetChunkCipher configures a single key (no rotation) — the common case.
func SetChunkCipher(key []byte) error { return SetChunkCiphers(key) }

// encryptText returns text ready to store: sentinel + primary key id + ciphertext
// when a cipher is configured, else the plaintext unchanged. Empty stays empty so
// nullable columns keep their NULL/empty semantics.
func encryptText(text string) (string, error) {
	if cipherPrimaryID == "" || text == "" {
		return text, nil
	}
	aead := cipherRing[cipherPrimaryID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, []byte(text), nil)
	return sentinelPrefix + cipherPrimaryID + "$" + base64.StdEncoding.EncodeToString(ct), nil
}

// decryptInto decrypts a scanned text field in place.
func decryptInto(p *string) error {
	v, err := decryptText(*p)
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// decryptText reverses encryptText, selecting the key by the id in the sentinel. A
// value without the sentinel is plaintext and returned unchanged. The legacy
// single-key format (id "v1") maps to the primary key. Ciphertext whose key id
// isn't in the ring (or with no key configured) is an error, not silent garbage.
func decryptText(stored string) (string, error) {
	if !strings.HasPrefix(stored, sentinelPrefix) {
		return stored, nil
	}
	if cipherPrimaryID == "" {
		return "", fmt.Errorf("value is encrypted but no ENCRYPTION_KEY is configured")
	}
	rest := strings.TrimPrefix(stored, sentinelPrefix)
	kid, b64, ok := strings.Cut(rest, "$")
	if !ok {
		return "", fmt.Errorf("malformed encrypted value")
	}
	if kid == legacyKeyID {
		kid = cipherPrimaryID // pre-rotation data was written with the (then only) key
	}
	aead := cipherRing[kid]
	if aead == nil {
		return "", fmt.Errorf("no encryption key for id %q (retired key not configured?)", kid)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode encrypted value: %w", err)
	}
	ns := aead.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("encrypted value too short")
	}
	pt, err := aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt value: %w", err)
	}
	return string(pt), nil
}

// reencryptValue re-writes a stored value under the current primary key (#403): a
// plaintext value gets encrypted, and ciphertext under any known (incl. retired)
// key is re-encrypted under the primary. Used by `kb reencrypt` to migrate off a
// rotated-out key. Empty stays empty. reencrypted reports whether the value changed.
func reencryptValue(stored string) (out string, reencrypted bool, err error) {
	if cipherPrimaryID == "" || stored == "" {
		return stored, false, nil
	}
	// Already under the primary key? Leave it (avoids rewriting the whole table).
	if strings.HasPrefix(stored, sentinelPrefix+cipherPrimaryID+"$") {
		return stored, false, nil
	}
	plain, err := decryptText(stored)
	if err != nil {
		return "", false, err
	}
	enc, err := encryptText(plain)
	if err != nil {
		return "", false, err
	}
	return enc, enc != stored, nil
}
