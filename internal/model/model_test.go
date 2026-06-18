package model_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// TestBotAvailable covers every branch of Bot.Available: disabled, rate-limited
// (UnavailableUntil in the future), expired rate limit, and a plain enabled bot.
func TestBotAvailable(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	cases := []struct {
		name string
		bot  model.Bot
		want bool
	}{
		{"disabled", model.Bot{Enabled: false}, false},
		{"disabled even when not parked", model.Bot{Enabled: false, UnavailableUntil: &past}, false},
		{"enabled, no park", model.Bot{Enabled: true}, true},
		{"enabled, parked in the future", model.Bot{Enabled: true, UnavailableUntil: &future}, false},
		{"enabled, park expired", model.Bot{Enabled: true, UnavailableUntil: &past}, true},
		{"enabled, park exactly now (now not before until)", model.Bot{Enabled: true, UnavailableUntil: &now}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.bot.Available(now); got != c.want {
				t.Errorf("Available(%v) = %v, want %v", now, got, c.want)
			}
		})
	}
}

// TestBlobStateReadable verifies only BlobStateStored is readable.
func TestBlobStateReadable(t *testing.T) {
	for _, s := range model.BlobStateValues() {
		want := s == model.BlobStateStored
		if got := s.Readable(); got != want {
			t.Errorf("BlobState(%s).Readable() = %v, want %v", s, got, want)
		}
	}
}

// TestPrincipalIsAdmin covers the nil-Auth, non-admin and admin cases.
func TestPrincipalIsAdmin(t *testing.T) {
	admin := &model.User{ID: uuid.New(), IsAdmin: true}
	plain := &model.User{ID: uuid.New()}

	cases := []struct {
		name string
		p    model.Principal
		want bool
	}{
		{"nil auth", model.Principal{Acting: plain, Auth: nil}, false},
		{"non-admin auth", model.Principal{Acting: plain, Auth: plain}, false},
		{"admin auth", model.Principal{Acting: plain, Auth: admin}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.IsAdmin(); got != c.want {
				t.Errorf("IsAdmin() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestPrincipalImpersonating covers direct auth vs admin/target impersonation.
func TestPrincipalImpersonating(t *testing.T) {
	admin := &model.User{ID: uuid.New(), IsAdmin: true}
	target := &model.User{ID: uuid.New()}

	cases := []struct {
		name string
		p    model.Principal
		want bool
	}{
		{"nil acting", model.Principal{Acting: nil, Auth: admin}, false},
		{"nil auth", model.Principal{Acting: target, Auth: nil}, false},
		{"same identity (direct auth)", model.Principal{Acting: target, Auth: target}, false},
		{"admin acting as target", model.Principal{Acting: target, Auth: admin}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.Impersonating(); got != c.want {
				t.Errorf("Impersonating() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestPrincipalContextRoundTrip verifies storing and extracting a principal, and
// the not-found case on a bare context.
func TestPrincipalContextRoundTrip(t *testing.T) {
	if _, ok := model.PrincipalFromContext(context.Background()); ok {
		t.Fatal("PrincipalFromContext on a bare context reported ok=true")
	}

	u := &model.User{ID: uuid.New(), Login: "u"}
	want := &model.Principal{Acting: u, Auth: u}
	ctx := model.ContextWithPrincipal(context.Background(), want)

	got, ok := model.PrincipalFromContext(ctx)
	if !ok {
		t.Fatal("PrincipalFromContext did not find the stored principal")
	}
	if got != want {
		t.Fatalf("PrincipalFromContext returned a different principal: %p vs %p", got, want)
	}
}

// TestDefaultSettings pins the built-in defaults so a regression in them is
// caught.
func TestDefaultSettings(t *testing.T) {
	s := model.DefaultSettings()
	if s.BlobMaxSize != 19*1024*1024 {
		t.Errorf("BlobMaxSize = %d, want %d", s.BlobMaxSize, 19*1024*1024)
	}
	if s.WALIdleTimeout != 60*time.Second {
		t.Errorf("WALIdleTimeout = %v, want 60s", s.WALIdleTimeout)
	}
	if s.MaxFileSize != 0 {
		t.Errorf("MaxFileSize = %d, want 0 (unlimited)", s.MaxFileSize)
	}
	if s.DefaultEvictionThreshold != 900_000 {
		t.Errorf("DefaultEvictionThreshold = %d, want 900000", s.DefaultEvictionThreshold)
	}
}
