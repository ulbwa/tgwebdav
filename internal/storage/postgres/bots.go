package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// botRepo implements domain.BotRepository. Tokens are encrypted at rest: the
// plaintext Bot.Token is never persisted, only its ciphertext (token_enc) and
// sha256 hex (token_sha). On read the token is decrypted back into Bot.Token.
type botRepo struct {
	base *gorm.DB
	enc  *encryptor
}

// hydrate decrypts a botModel into a domain.Bot. A decryption failure (e.g.
// missing key) surfaces as an error so callers do not act on an empty token.
func (r *botRepo) hydrate(m *botModel) (*domain.Bot, error) {
	token, err := r.enc.decrypt(m.TokenEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt bot token: %w", err)
	}
	return &domain.Bot{
		ID:               m.ID,
		Username:         m.Username,
		Token:            token,
		Enabled:          m.Enabled,
		UnavailableUntil: m.UnavailableUntil,
		CreatedAt:        m.CreatedAt,
	}, nil
}

// Create encrypts the bot token and inserts the row.
func (r *botRepo) Create(ctx context.Context, b *domain.Bot) error {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	enc, err := r.enc.encrypt(b.Token)
	if err != nil {
		return fmt.Errorf("create bot: %w", err)
	}
	m := &botModel{
		ID:               b.ID,
		Username:         b.Username,
		TokenSHA:         tokenSHA(b.Token),
		TokenEnc:         enc,
		Enabled:          b.Enabled,
		UnavailableUntil: b.UnavailableUntil,
		CreatedAt:        b.CreatedAt,
	}
	if err := txFromCtx(ctx, r.base).Create(m).Error; err != nil {
		return fmt.Errorf("create bot: %w", translateError(err))
	}
	return nil
}

// Update re-encrypts the token and saves the mutable columns.
func (r *botRepo) Update(ctx context.Context, b *domain.Bot) error {
	enc, err := r.enc.encrypt(b.Token)
	if err != nil {
		return fmt.Errorf("update bot: %w", err)
	}
	res := txFromCtx(ctx, r.base).Model(&botModel{}).
		Where("id = ?", b.ID).
		Updates(map[string]any{
			"username":          b.Username,
			"token_sha":         tokenSHA(b.Token),
			"token_enc":         enc,
			"enabled":           b.Enabled,
			"unavailable_until": b.UnavailableUntil,
		})
	if res.Error != nil {
		return fmt.Errorf("update bot: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update bot: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes a bot by id (cascades to bot_channel and blob_bot_files).
func (r *botRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res := txFromCtx(ctx, r.base).Where("id = ?", id).Delete(&botModel{})
	if res.Error != nil {
		return fmt.Errorf("delete bot: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete bot: %w", domain.ErrNotFound)
	}
	return nil
}

// GetByID loads and decrypts a bot by primary key.
func (r *botRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Bot, error) {
	var m botModel
	if err := txFromCtx(ctx, r.base).Where("id = ?", id).First(&m).Error; err != nil {
		return nil, fmt.Errorf("get bot by id: %w", translateError(err))
	}
	return r.hydrate(&m)
}

// GetByUsername loads and decrypts a bot by its username.
func (r *botRepo) GetByUsername(ctx context.Context, username string) (*domain.Bot, error) {
	var m botModel
	if err := txFromCtx(ctx, r.base).Where("username = ?", username).First(&m).Error; err != nil {
		return nil, fmt.Errorf("get bot by username: %w", translateError(err))
	}
	return r.hydrate(&m)
}

// List returns all bots (decrypted), ordered by creation time.
func (r *botRepo) List(ctx context.Context) ([]domain.Bot, error) {
	var ms []botModel
	if err := txFromCtx(ctx, r.base).Order("created_at").Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("list bots: %w", translateError(err))
	}
	out := make([]domain.Bot, len(ms))
	for i := range ms {
		b, err := r.hydrate(&ms[i])
		if err != nil {
			return nil, fmt.Errorf("list bots: %w", err)
		}
		out[i] = *b
	}
	return out, nil
}

// SetUnavailableUntil records (or clears, with nil) a bot's retry-after window.
func (r *botRepo) SetUnavailableUntil(ctx context.Context, id uuid.UUID, until *time.Time) error {
	res := txFromCtx(ctx, r.base).Model(&botModel{}).
		Where("id = ?", id).
		Update("unavailable_until", until)
	if res.Error != nil {
		return fmt.Errorf("set bot unavailable: %w", translateError(res.Error))
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set bot unavailable: %w", domain.ErrNotFound)
	}
	return nil
}
