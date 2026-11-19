package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// rsaBits is the size of signing keys. RS256 is the algorithm every OpenID Connect
// client supports.
const rsaBits = 2048

// signingKey is a private key that signs ID tokens.
type signingKey struct {
	id  string
	key *rsa.PrivateKey
}

func (k signingKey) SignatureAlgorithm() jose.SignatureAlgorithm { return jose.RS256 }
func (k signingKey) Key() any                                    { return k.key }
func (k signingKey) ID() string                                  { return k.id }

// publicKey is a key the key set publishes.
type publicKey struct {
	id  string
	key *rsa.PublicKey
}

func (k publicKey) ID() string                         { return k.id }
func (k publicKey) Algorithm() jose.SignatureAlgorithm { return jose.RS256 }
func (k publicKey) Use() string                        { return "sig" }
func (k publicKey) Key() any                           { return k.key }

// signingKey returns the key that signs now. It makes one if there is none: on the first
// sign-in, or after the secret key was replaced.
func (p *Provider) signingKey(ctx context.Context) (op.SigningKey, error) {
	for range 2 {
		var id string
		var sealed []byte
		err := p.app.DB().QueryRowContext(ctx, `
			SELECT id, private_key
			FROM oidc_keys
			WHERE sealed_with = $1 AND signs_from <= now() AND signs_until > now()
			ORDER BY signs_from DESC
			LIMIT 1`, p.keyCheck,
		).Scan(&id, &sealed)
		if errors.Is(err, sql.ErrNoRows) {
			if err := p.ensureKeys(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		return p.openKey(id, sealed)
	}
	return nil, errors.New("oidc: no signing key")
}

// keySet returns the published keys: from when each was made, at least keyOverlap
// before it signs, until keyOverlap after it stops.
func (p *Provider) keySet(ctx context.Context) ([]op.Key, error) {
	for range 2 {
		keys, err := p.publishedKeys(ctx)
		if err != nil || len(keys) > 0 {
			return keys, err
		}
		if err := p.ensureKeys(ctx); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("oidc: no signing key")
}

func (p *Provider) publishedKeys(ctx context.Context) (_ []op.Key, err error) {
	rows, err := p.app.DB().QueryContext(ctx, `
		SELECT id, public_key
		FROM oidc_keys
		WHERE signs_until > now() - make_interval(secs => $1)
		ORDER BY signs_from`, keyOverlap.Seconds(),
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	var keys []op.Key
	for rows.Next() {
		var id string
		var der []byte
		if err := rows.Scan(&id, &der); err != nil {
			return nil, err
		}
		pub, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return nil, fmt.Errorf("oidc: key %s: %w", id, err)
		}
		rsaPub, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("oidc: key %s is not RSA", id)
		}
		keys = append(keys, publicKey{id: id, key: rsaPub})
	}
	return keys, rows.Err()
}

// ensureKeys makes sure a key signs now and the next one is published ahead of its
// turn. Replicas take turns, under an advisory lock. It looks only at keys sealed with
// the current secret key, so replacing the secret key starts a new key at once.
func (p *Provider) ensureKeys(ctx context.Context) error {
	tx, err := p.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('oidc-keys', 0))`); err != nil {
		return err
	}
	var now time.Time
	var last sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT now(), max(signs_until)
		FROM oidc_keys
		WHERE sealed_with = $1`, p.keyCheck,
	).Scan(&now, &last)
	if err != nil {
		return err
	}
	end := last.Time
	if !last.Valid || !end.After(now) {
		if err := p.addKey(ctx, tx, now); err != nil {
			return err
		}
		end = now.Add(keyLifetime)
	}
	// The job runs hourly; starting the next key two overlaps ahead publishes it at least
	// one overlap before it signs, even if the job misses runs for a day.
	if end.Before(now.Add(2 * keyOverlap)) {
		if err := p.addKey(ctx, tx, end); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// addKey makes a key that signs from from, for keyLifetime.
func (p *Provider) addKey(ctx context.Context, tx *sql.Tx, from time.Time) error {
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	idBytes := make([]byte, 16)
	_, _ = rand.Read(idBytes) // crypto/rand never fails
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO oidc_keys (id, public_key, private_key, sealed_with, signs_from, signs_until)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, pub, seal(p.sealKey, keyLabel(id), priv), p.keyCheck, from, from.Add(keyLifetime),
	)
	return err
}

// openKey unseals a private key.
func (p *Provider) openKey(id string, sealed []byte) (signingKey, error) {
	der, err := open(p.sealKey, keyLabel(id), sealed)
	if err != nil {
		return signingKey{}, fmt.Errorf("oidc: key %s does not open", id)
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return signingKey{}, fmt.Errorf("oidc: key %s: %w", id, err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return signingKey{}, fmt.Errorf("oidc: key %s is not RSA", id)
	}
	return signingKey{id: id, key: rsaKey}, nil
}

// keyLabel ties a sealed private key to its ID, so keys can't be swapped between rows.
func keyLabel(id string) string { return "oidc-signing-key:" + id }

// maintain runs hourly: it starts the next signing key ahead of its turn, and deletes
// expired codes, access tokens and keys. A code stays until its access tokens have
// expired, so a second use of it is still caught.
func (p *Provider) maintain(ctx context.Context) error {
	if err := p.ensureKeys(ctx); err != nil {
		return err
	}
	db := p.app.DB()
	if _, err := db.ExecContext(ctx, `
		DELETE FROM oidc_codes
		WHERE expires_at < now() - make_interval(secs => $1)`, accessTokenLifetime.Seconds(),
	); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		DELETE FROM oidc_access_tokens
		WHERE expires_at < now()`,
	); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		DELETE FROM oidc_keys
		WHERE signs_until < now() - make_interval(secs => $1)`, keyOverlap.Seconds(),
	)
	return err
}
