package repository

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// errNoSecretKey is returned by the token cipher when a bot token operation is
// attempted but the repository was constructed without a secret key.
var errNoSecretKey = errors.New("repository: no secret key configured for bot tokens")

// BotRepository persists Telegram bots. Tokens are encrypted at rest with
// AES-256-GCM: the plaintext model.Bot.Token is never stored, only its
// ciphertext (token_enc, a 12-byte random nonce prepended to the GCM seal) and
// its sha256 hex digest (token_sha, for idempotent lookup/dedup). On read the
// token is decrypted back into model.Bot.Token.
type BotRepository struct {
	pool *pgxpool.Pool
	aead cipher.AEAD // nil when no key was supplied
}

// NewBotRepository builds a BotRepository bound to pool. secretKey must be a
// 32-byte AES-256 key, or empty to disable token operations (which then fail
// with errNoSecretKey). The AES-GCM construction is byte-compatible with the
// legacy postgres layer, so ciphertext written by either side is readable by
// the other.
func NewBotRepository(pool *pgxpool.Pool, secretKey []byte) *BotRepository {
	r := &BotRepository{pool: pool}
	if len(secretKey) == 0 {
		return r
	}
	if len(secretKey) != 32 {
		// Mirror the legacy contract: a wrong-length key disables the cipher,
		// surfacing as errNoSecretKey on the first token operation rather than
		// panicking at construction. Callers validate key length up front.
		return r
	}
	block, err := aes.NewCipher(secretKey)
	if err != nil {
		return r
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return r
	}
	r.aead = aead
	return r
}

// tokenSHA returns the sha256 hex digest of token.
func tokenSHA(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// encryptToken seals token, returning nonce||ciphertext.
func (r *BotRepository) encryptToken(token string) ([]byte, error) {
	if r.aead == nil {
		return nil, errNoSecretKey
	}
	nonce := make([]byte, r.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	ct := r.aead.Seal(nil, nonce, []byte(token), nil)
	return append(nonce, ct...), nil
}

// decryptToken opens a nonce||ciphertext blob produced by encryptToken.
func (r *BotRepository) decryptToken(enc []byte) (string, error) {
	if r.aead == nil {
		return "", errNoSecretKey
	}
	ns := r.aead.NonceSize()
	if len(enc) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := enc[:ns], enc[ns:]
	pt, err := r.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("gcm open: %w", err)
	}
	return string(pt), nil
}

// hydrate decrypts a sqlc.Bot row into a model.Bot. A decryption failure (e.g.
// missing key) surfaces as an error so callers do not act on an empty token.
func (r *BotRepository) hydrate(m sqlc.Bot) (*model.Bot, error) {
	token, err := r.decryptToken(m.TokenEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt bot token: %w", err)
	}
	return &model.Bot{
		ID:               m.ID,
		Username:         m.Username,
		Token:            token,
		Enabled:          m.Enabled,
		UnavailableUntil: timeToPtr(m.UnavailableUntil),
		CreatedAt:        m.CreatedAt.Time,
	}, nil
}

// Create encrypts the bot token and inserts the row.
func (r *BotRepository) Create(ctx context.Context, b *model.Bot) error {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	enc, err := r.encryptToken(b.Token)
	if err != nil {
		return fmt.Errorf("create bot: %w", err)
	}
	db := database.FromContext(ctx, r.pool)
	err = sqlc.New(db).CreateBot(ctx, sqlc.CreateBotParams{
		ID:               b.ID,
		Username:         b.Username,
		TokenSha:         tokenSHA(b.Token),
		TokenEnc:         enc,
		Enabled:          b.Enabled,
		UnavailableUntil: ptrToTime(b.UnavailableUntil),
		CreatedAt:        ptrToTime(&b.CreatedAt),
	})
	if err != nil {
		return fmt.Errorf("create bot: %w", translateError(err))
	}
	return nil
}

// Update re-encrypts the token and saves the mutable columns.
func (r *BotRepository) Update(ctx context.Context, b *model.Bot) error {
	enc, err := r.encryptToken(b.Token)
	if err != nil {
		return fmt.Errorf("update bot: %w", err)
	}
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).UpdateBot(ctx, sqlc.UpdateBotParams{
		ID:               b.ID,
		Username:         b.Username,
		TokenSha:         tokenSHA(b.Token),
		TokenEnc:         enc,
		Enabled:          b.Enabled,
		UnavailableUntil: ptrToTime(b.UnavailableUntil),
	})
	if err != nil {
		return fmt.Errorf("update bot: %w", translateError(err))
	}
	if rows == 0 {
		return fmt.Errorf("update bot: %w", model.ErrNotFound)
	}
	return nil
}

// Delete removes a bot by id (cascades to bot_channel and blob_bot_files).
func (r *BotRepository) Delete(ctx context.Context, id uuid.UUID) error {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).DeleteBot(ctx, id)
	if err != nil {
		return fmt.Errorf("delete bot: %w", translateError(err))
	}
	if rows == 0 {
		return fmt.Errorf("delete bot: %w", model.ErrNotFound)
	}
	return nil
}

// GetByID loads and decrypts a bot by primary key.
func (r *BotRepository) GetByID(ctx context.Context, id uuid.UUID) (*model.Bot, error) {
	db := database.FromContext(ctx, r.pool)
	m, err := sqlc.New(db).GetBotByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get bot by id: %w", translateError(err))
	}
	return r.hydrate(m)
}

// GetByUsername loads and decrypts a bot by its username.
func (r *BotRepository) GetByUsername(ctx context.Context, username string) (*model.Bot, error) {
	db := database.FromContext(ctx, r.pool)
	m, err := sqlc.New(db).GetBotByUsername(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("get bot by username: %w", translateError(err))
	}
	return r.hydrate(m)
}

// List returns all bots (decrypted), ordered by creation time.
func (r *BotRepository) List(ctx context.Context) ([]model.Bot, error) {
	db := database.FromContext(ctx, r.pool)
	ms, err := sqlc.New(db).ListBots(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", translateError(err))
	}
	out := make([]model.Bot, len(ms))
	for i := range ms {
		b, err := r.hydrate(ms[i])
		if err != nil {
			return nil, fmt.Errorf("list bots: %w", err)
		}
		out[i] = *b
	}
	return out, nil
}

// SetUnavailableUntil records (or clears, with nil) a bot's retry-after window.
func (r *BotRepository) SetUnavailableUntil(ctx context.Context, id uuid.UUID, until *time.Time) error {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).SetBotUnavailableUntil(ctx, sqlc.SetBotUnavailableUntilParams{
		ID:               id,
		UnavailableUntil: ptrToTime(until),
	})
	if err != nil {
		return fmt.Errorf("set bot unavailable: %w", translateError(err))
	}
	if rows == 0 {
		return fmt.Errorf("set bot unavailable: %w", model.ErrNotFound)
	}
	return nil
}
