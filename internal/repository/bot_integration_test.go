package repository

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/model"
)

// testSecretKey returns a deterministic 32-byte AES-256 key for tests.
func testSecretKey() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

func TestBotRepository_RoundTripToken(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotRepository(pool, testSecretKey())

	const plaintext = "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"
	b := &model.Bot{Username: "round_trip_bot", Token: plaintext, Enabled: true}
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.ID == uuid.Nil {
		t.Fatal("Create did not assign an ID")
	}

	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Token != plaintext {
		t.Fatalf("decrypted token = %q, want %q", got.Token, plaintext)
	}
	if got.Username != "round_trip_bot" || !got.Enabled {
		t.Fatalf("round-trip lost fields: %+v", got)
	}

	// The stored token_enc column must NOT be the plaintext, and token_sha must
	// be the sha256 hex of the plaintext token.
	var tokenEnc []byte
	var tokenSha string
	err = pool.QueryRow(ctx,
		"SELECT token_enc, token_sha FROM bots WHERE id = $1", b.ID,
	).Scan(&tokenEnc, &tokenSha)
	if err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if bytes.Equal(tokenEnc, []byte(plaintext)) {
		t.Fatal("token_enc stored as plaintext")
	}
	if bytes.Contains(tokenEnc, []byte(plaintext)) {
		t.Fatal("token_enc contains the plaintext token")
	}
	if want := tokenSHA(plaintext); tokenSha != want {
		t.Fatalf("token_sha = %q, want %q", tokenSha, want)
	}
	// AES-256-GCM with a 12-byte nonce prepended: ciphertext = nonce(12) +
	// plaintext + tag(16).
	if wantLen := 12 + len(plaintext) + 16; len(tokenEnc) != wantLen {
		t.Fatalf("token_enc len = %d, want %d", len(tokenEnc), wantLen)
	}
}

func TestBotRepository_EncryptionIsRandomized(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotRepository(pool, testSecretKey())

	// Two bots with the SAME token must yield different token_enc (random nonce)
	// but the same token_sha.
	b1 := &model.Bot{Username: "dup1", Token: "same-token", Enabled: true}
	b2 := &model.Bot{Username: "dup2", Token: "same-token", Enabled: true}
	if err := repo.Create(ctx, b1); err != nil {
		t.Fatalf("Create b1: %v", err)
	}
	// token_sha is UNIQUE, so the duplicate token must collide.
	err := repo.Create(ctx, b2)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Create duplicate token error = %v, want ErrAlreadyExists", err)
	}
}

func TestBotRepository_GetByUsernameAndList(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotRepository(pool, testSecretKey())

	a := &model.Bot{Username: "alpha", Token: "tok-a", Enabled: true,
		CreatedAt: time.Now().Add(-2 * time.Hour)}
	c := &model.Bot{Username: "charlie", Token: "tok-c", Enabled: false,
		CreatedAt: time.Now().Add(-1 * time.Hour)}
	for _, b := range []*model.Bot{a, c} {
		if err := repo.Create(ctx, b); err != nil {
			t.Fatalf("Create %s: %v", b.Username, err)
		}
	}

	got, err := repo.GetByUsername(ctx, "charlie")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if got.Token != "tok-c" || got.Enabled {
		t.Fatalf("GetByUsername returned %+v", got)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2", len(list))
	}
	// Ordered by created_at: alpha first.
	if list[0].Username != "alpha" || list[1].Username != "charlie" {
		t.Fatalf("List order = %s,%s", list[0].Username, list[1].Username)
	}
	if list[0].Token != "tok-a" {
		t.Fatalf("List did not decrypt token: %q", list[0].Token)
	}
}

func TestBotRepository_UpdateReEncrypts(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotRepository(pool, testSecretKey())

	b := &model.Bot{Username: "upd", Token: "old-token", Enabled: true}
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	b.Token = "new-token"
	b.Username = "upd2"
	b.Enabled = false
	if err := repo.Update(ctx, b); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Token != "new-token" || got.Username != "upd2" || got.Enabled {
		t.Fatalf("Update did not persist: %+v", got)
	}

	var tokenSha string
	if err := pool.QueryRow(ctx,
		"SELECT token_sha FROM bots WHERE id = $1", b.ID).Scan(&tokenSha); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if want := tokenSHA("new-token"); tokenSha != want {
		t.Fatalf("token_sha after update = %q, want %q", tokenSha, want)
	}
}

func TestBotRepository_SetUnavailableUntil(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotRepository(pool, testSecretKey())

	b := &model.Bot{Username: "ua", Token: "tok", Enabled: true}
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	until := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	if err := repo.SetUnavailableUntil(ctx, b.ID, &until); err != nil {
		t.Fatalf("SetUnavailableUntil: %v", err)
	}
	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.UnavailableUntil == nil {
		t.Fatal("UnavailableUntil not set")
	}
	if !got.UnavailableUntil.Equal(until) {
		t.Fatalf("UnavailableUntil = %v, want %v", got.UnavailableUntil, until)
	}

	// Clearing with nil.
	if err := repo.SetUnavailableUntil(ctx, b.ID, nil); err != nil {
		t.Fatalf("SetUnavailableUntil(nil): %v", err)
	}
	got, err = repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetByID after clear: %v", err)
	}
	if got.UnavailableUntil != nil {
		t.Fatalf("UnavailableUntil not cleared: %v", got.UnavailableUntil)
	}
}

func TestBotRepository_NotFound(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotRepository(pool, testSecretKey())

	missing := uuid.New()
	if _, err := repo.GetByID(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID missing = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByUsername(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByUsername missing = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing = %v, want ErrNotFound", err)
	}
	if err := repo.SetUnavailableUntil(ctx, missing, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetUnavailableUntil missing = %v, want ErrNotFound", err)
	}
	upd := &model.Bot{ID: missing, Username: "x", Token: "t"}
	if err := repo.Update(ctx, upd); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing = %v, want ErrNotFound", err)
	}
}

func TestBotRepository_Delete(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotRepository(pool, testSecretKey())

	b := &model.Bot{Username: "del", Token: "tok", Enabled: true}
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Delete(ctx, b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID after delete = %v, want ErrNotFound", err)
	}
}

func TestBotRepository_NoSecretKeyFailsTokenOps(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotRepository(pool, nil) // no key

	b := &model.Bot{Username: "nokey", Token: "tok", Enabled: true}
	if err := repo.Create(ctx, b); !errors.Is(err, errNoSecretKey) {
		t.Fatalf("Create without key = %v, want errNoSecretKey", err)
	}
}

func TestBotRepository_DecryptsLegacyCiphertext(t *testing.T) {
	// Proves byte-compatibility: a row whose token_enc was produced by the same
	// AES-256-GCM (nonce||seal) construction is decrypted back to plaintext.
	pool := setupTestPool(t)
	ctx := context.Background()
	repo := NewBotRepository(pool, testSecretKey())

	const plaintext = "legacy-token-value"
	// Seal exactly as the cipher does, independently, then insert the raw row.
	enc, err := repo.encryptToken(plaintext)
	if err != nil {
		t.Fatalf("encryptToken: %v", err)
	}
	id := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO bots (id, username, token_sha, token_enc, enabled, created_at)
		 VALUES ($1, $2, $3, $4, $5, now())`,
		id, "legacy", tokenSHA(plaintext), enc, true,
	)
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}

	got, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Token != plaintext {
		t.Fatalf("legacy decrypt = %q, want %q", got.Token, plaintext)
	}
}
