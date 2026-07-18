package store

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/programmism/brainiac/internal/model"
)

func TestChunkCipherRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	if err := SetChunkCipher(key); err != nil {
		t.Fatalf("set cipher: %v", err)
	}
	defer func() { _ = SetChunkCipher(nil) }()

	const plain = "OrderService writes to Kafka for durability."
	enc, err := encryptText(plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !strings.HasPrefix(enc, chunkTextSentinel) {
		t.Fatalf("ciphertext missing sentinel: %q", enc)
	}
	if strings.Contains(enc, plain) {
		t.Fatal("plaintext leaked into ciphertext")
	}
	got, err := decryptText(enc)
	if err != nil || got != plain {
		t.Fatalf("round-trip = (%q, %v), want %q", got, err, plain)
	}
	// A plaintext row (no sentinel) passes through unchanged even with a key set.
	if got, err := decryptText(plain); err != nil || got != plain {
		t.Fatalf("plaintext passthrough = (%q, %v), want %q", got, err, plain)
	}
	// Two encryptions of the same text differ (random nonce).
	enc2, _ := encryptText(plain)
	if enc == enc2 {
		t.Fatal("nonce reuse: identical ciphertext for repeated encryption")
	}
}

func TestChunkCipherDisabledAndErrors(t *testing.T) {
	_ = SetChunkCipher(nil) // disabled (default)
	const plain = "hello"
	if enc, err := encryptText(plain); err != nil || enc != plain {
		t.Fatalf("disabled encrypt = (%q, %v), want passthrough", enc, err)
	}
	// Ciphertext read back with no key configured is an error, not silent garbage.
	if _, err := decryptText(chunkTextSentinel + "Zm9v"); err == nil {
		t.Fatal("expected error decrypting ciphertext with no key")
	}
	// Wrong key length is rejected.
	if err := SetChunkCipher([]byte("short")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
	_ = SetChunkCipher(nil)
}

// TestChunkTextEncryptedAtRest is the store round-trip (#377): with a cipher set,
// the stored text column is ciphertext while reads return plaintext; a row written
// as plaintext (cipher off) is still readable after the cipher is enabled.
func TestChunkTextEncryptedAtRest(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed encryption test")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE chunk_sources, chunks CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// A pre-encryption plaintext row.
	plainChunk := &model.Chunk{Text: "plaintext era chunk", Embedding: vec(1), SourceURI: "s://plain", ContentHash: "hp"}
	if err := InsertChunk(ctx, pool, plainChunk); err != nil {
		t.Fatalf("insert plaintext: %v", err)
	}

	// Enable encryption and store a second chunk.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	if err := SetChunkCipher(key); err != nil {
		t.Fatalf("set cipher: %v", err)
	}
	defer func() { _ = SetChunkCipher(nil) }()

	const secret = "SECRET OrderService writes to Kafka nightly for durability."
	encChunk := &model.Chunk{Text: secret, Embedding: vec(2), SourceURI: "s://enc", ContentHash: "he"}
	if err := InsertChunk(ctx, pool, encChunk); err != nil {
		t.Fatalf("insert encrypted: %v", err)
	}

	// The raw column is ciphertext (sentinel-prefixed, no plaintext).
	var raw string
	if err := pool.QueryRow(ctx, `SELECT text FROM chunks WHERE source_uri = 's://enc'`).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !strings.HasPrefix(raw, chunkTextSentinel) || strings.Contains(raw, "OrderService") {
		t.Fatalf("stored text not encrypted at rest: %q", raw)
	}

	// Reads decrypt transparently — and the plaintext-era row still reads fine.
	got, err := GetChunksBySourceURI(ctx, pool, "s://enc", 10, NoWall())
	if err != nil || len(got) != 1 || got[0].Text != secret {
		t.Fatalf("encrypted read = %+v, %v; want text %q", got, err, secret)
	}
	gotPlain, err := GetChunksBySourceURI(ctx, pool, "s://plain", 10, NoWall())
	if err != nil || len(gotPlain) != 1 || gotPlain[0].Text != "plaintext era chunk" {
		t.Fatalf("plaintext-era read = %+v, %v", gotPlain, err)
	}
}
