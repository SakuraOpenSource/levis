package model

import "time"

// RevokedToken is the durable logout revocation record (P-AUTH-4).
//
// JWT sessions are stateless; logout only removes the browser cookie while
// the token itself stays valid until expiry. Revocation must therefore be a
// server-side fact: persisting it (rather than keeping it in process memory)
// means a restart or another replica behind the same database can no longer
// resurrect a token the user explicitly logged out.
type RevokedToken struct {
	// JTI is the JWT ID: the only handle that identifies one specific token.
	JTI string `gorm:"primaryKey;size:64" json:"jti"`
	// ExpiresAt is the token's natural expiry; the row is meaningless after
	// it and is removed by the periodic cleanup.
	ExpiresAt time.Time `gorm:"index" json:"expires_at"`
}
