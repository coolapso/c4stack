// Package apikeys issues API keys to accounts and authenticates API requests with them.
//
// A key is the app's prefix, an underscore and 32 random bytes in base64url, such as
// acme_V3kz…. Only the SHA-256 of the whole key is stored, with its last characters for
// display. The raw key is shown once, when it is created, and never logged. Lookup is
// by hash, so changing the prefix never invalidates existing keys.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/app"
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations create the api_keys table. List them after accounts.Migrations.
var Migrations = app.Migrations{Namespace: "apikeys", Files: sub(migrationFiles, "migrations")}

func sub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}

var (
	// ErrNotFound means the account has no key with that ID.
	ErrNotFound = errors.New("apikeys: key not found")
	// ErrInvalidKey means a presented key is malformed, unknown or revoked.
	ErrInvalidKey = errors.New("apikeys: invalid key")
	// ErrInvalidName means a key name is empty, too long or contains control characters.
	ErrInvalidName = errors.New("apikeys: a key name needs 1 to 64 printable characters")
	// ErrActive means a key must be revoked before it can be deleted.
	ErrActive = errors.New("apikeys: revoke the key before deleting it")
)

// Config configures the module.
type Config struct {
	// Prefix starts every new key, such as "acme" for acme_… keys. Letters, digits and
	// underscores, starting with a letter.
	Prefix string
}

// Key describes an issued key. It never holds the raw key.
type Key struct {
	ID        string
	AccountID string
	Name      string
	// Prefix is the module's current prefix, for display.
	Prefix     string
	Suffix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Display returns a recognisable, non-secret form of the key, such as acme_••••x7Qa9z.
func (k Key) Display() string {
	return k.Prefix + "_••••" + k.Suffix
}

// Keys is the API keys module.
type Keys struct {
	app    *app.App
	prefix string
}

var prefixPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,23}$`)

// New returns the API keys module. Keys belong to accounts, and a foreign key deletes
// them with their account.
func New(a *app.App, cfg Config) (*Keys, error) {
	for _, m := range []app.Migrations{accounts.Migrations, Migrations} {
		if err := a.Require(m); err != nil {
			return nil, err
		}
	}
	if !prefixPattern.MatchString(cfg.Prefix) {
		return nil, errors.New("apikeys: the prefix needs 1 to 24 letters, digits or underscores, starting with a letter")
	}
	return &Keys{app: a, prefix: cfg.Prefix}, nil
}

const (
	secretBytes  = 32
	secretLength = 43 // base64url of 32 bytes, without padding
	suffixLength = 6
)

// validName trims a key name and checks it.
func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 64 {
		return "", ErrInvalidName
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", ErrInvalidName
		}
	}
	return name, nil
}

// Create issues a key and returns it raw, once. Store or show it immediately.
func (k *Keys) Create(ctx context.Context, accountID, name string) (string, Key, error) {
	name, err := validName(name)
	if err != nil {
		return "", Key{}, err
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", Key{}, err
	}
	raw := k.prefix + "_" + base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(raw))
	key := Key{AccountID: accountID, Name: name, Prefix: k.prefix, Suffix: raw[len(raw)-suffixLength:]}
	err = k.app.DB().QueryRowContext(ctx, `
		INSERT INTO api_keys (account_id, name, key_hash, key_suffix)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`,
		accountID, name, hash[:], key.Suffix,
	).Scan(&key.ID, &key.CreatedAt)
	if err != nil {
		return "", Key{}, fmt.Errorf("apikeys: create: %w", err)
	}
	return raw, key, nil
}

// List returns an account's keys, newest first, revoked ones included.
func (k *Keys) List(ctx context.Context, accountID string) ([]Key, error) {
	rows, err := k.app.DB().QueryContext(ctx, `
		SELECT id, account_id, name, key_suffix, created_at, last_used_at, revoked_at
		FROM api_keys
		WHERE account_id = $1
		ORDER BY created_at DESC, id`, accountID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []Key
	for rows.Next() {
		key := Key{Prefix: k.prefix}
		if err := rows.Scan(&key.ID, &key.AccountID, &key.Name, &key.Suffix, &key.CreatedAt, &key.LastUsedAt, &key.RevokedAt); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Rename renames an active key. A revoked or unknown key returns ErrNotFound.
func (k *Keys) Rename(ctx context.Context, accountID, keyID, name string) error {
	name, err := validName(name)
	if err != nil {
		return err
	}
	var id string
	err = k.app.DB().QueryRowContext(ctx, `
		UPDATE api_keys
		SET name = $3
		WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL
		RETURNING id`, keyID, accountID, name,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return ErrNotFound
	}
	return err
}

// Delete removes a revoked key from the account's list. An active key returns
// ErrActive; a key that no longer exists counts as deleted.
func (k *Keys) Delete(ctx context.Context, accountID, keyID string) error {
	var active bool
	err := k.app.DB().QueryRowContext(ctx, `
		WITH deleted AS (
		    DELETE FROM api_keys
		    WHERE id = $1 AND account_id = $2 AND revoked_at IS NOT NULL
		    RETURNING id
		)
		SELECT EXISTS (
		    SELECT 1
		    FROM api_keys
		    WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL
		)`, keyID, accountID,
	).Scan(&active)
	switch {
	case isInvalidUUID(err):
		return nil
	case err != nil:
		return err
	case active:
		return ErrActive
	}
	return nil
}

// LockActive checks inside tx that the key still authenticates for the account, and
// keeps it from being revoked until tx ends. Run it in the transaction that spends
// credit for a request, so a key revoked meanwhile cannot spend. It returns
// ErrInvalidKey for a revoked or unknown key.
func (k *Keys) LockActive(ctx context.Context, tx *sql.Tx, accountID, keyID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM api_keys
		WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL
		FOR SHARE`, keyID, accountID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return ErrInvalidKey
	}
	return err
}

// Revoke stops a key from authenticating. Revoking a revoked key changes nothing.
// An unknown key returns ErrNotFound.
func (k *Keys) Revoke(ctx context.Context, accountID, keyID string) error {
	var id string
	err := k.app.DB().QueryRowContext(ctx, `
		UPDATE api_keys
		SET revoked_at = COALESCE(revoked_at, now())
		WHERE id = $1 AND account_id = $2
		RETURNING id`, keyID, accountID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return ErrNotFound
	}
	return err
}

// Authenticate returns the active key matching raw. It records when the key was last
// used, at most every five minutes.
func (k *Keys) Authenticate(ctx context.Context, raw string) (Key, error) {
	if !wellFormed(raw) {
		return Key{}, ErrInvalidKey
	}
	hash := sha256.Sum256([]byte(raw))
	key := Key{Prefix: k.prefix}
	err := k.app.DB().QueryRowContext(ctx, `
		SELECT id, account_id, name, key_suffix, created_at, last_used_at
		FROM api_keys
		WHERE key_hash = $1 AND revoked_at IS NULL`, hash[:],
	).Scan(&key.ID, &key.AccountID, &key.Name, &key.Suffix, &key.CreatedAt, &key.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrInvalidKey
	}
	if err != nil {
		return Key{}, err
	}
	if key.LastUsedAt == nil || time.Since(*key.LastUsedAt) > 5*time.Minute {
		if _, err := k.app.DB().ExecContext(ctx, `
			UPDATE api_keys
			SET last_used_at = statement_timestamp()
			WHERE id = $1
			    AND (last_used_at IS NULL OR last_used_at < statement_timestamp() - interval '5 minutes')`, key.ID,
		); err != nil {
			return Key{}, err
		}
	}
	return key, nil
}

// wellFormed checks a presented key's shape before it reaches the database: a prefix,
// an underscore and exactly 32 bytes in canonical base64url.
func wellFormed(raw string) bool {
	if len(raw) < secretLength+2 || len(raw) > secretLength+25 || raw[len(raw)-secretLength-1] != '_' {
		return false
	}
	if !prefixPattern.MatchString(raw[:len(raw)-secretLength-1]) {
		return false
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(raw[len(raw)-secretLength:])
	return err == nil && len(secret) == secretBytes
}

// isInvalidUUID reports PostgreSQL's error for a malformed UUID parameter.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
