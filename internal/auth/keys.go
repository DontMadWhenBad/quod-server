package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver ("sqlite")
)

// Server-issued access keys, stored in SQLite.
//
// A key is a random secret ("qk_" + 52 base32 chars, 256 bits). Only its
// SHA-256 is stored; a slow password hash is unnecessary because the secret
// has full entropy. Requests are checked against the database directly, so
// a revoked key stops working on the next request. The admin tool and the
// server may use the same database concurrently (WAL mode, busy timeout).

const keyPrefix = "qk_"

var keyEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Key is one issued key (never containing the secret itself).
type Key struct {
	ID             string     `json:"id"` // public, e.g. k_3f9a1c2b
	Name           string     `json:"name"`
	Created        time.Time  `json:"created"`
	Revoked        *time.Time `json:"revoked,omitempty"`
	DailyBudgetUSD float64    `json:"daily_budget_usd,omitempty"`
}

// Active reports whether the key may be used.
func (k Key) Active() bool { return k.Revoked == nil }

// Principal is the identity a key authenticates as.
func (k Key) Principal() *Principal {
	return &Principal{ID: "key:" + k.ID, Name: k.Name, DailyBudgetUSD: k.DailyBudgetUSD}
}

// migrations[i] upgrades the schema from version i to i+1 (PRAGMA user_version).
var migrations = []string{
	`CREATE TABLE api_keys (
		id               TEXT PRIMARY KEY,
		name             TEXT NOT NULL,
		hash             TEXT NOT NULL UNIQUE,
		created_at       INTEGER NOT NULL,   -- unix seconds
		revoked_at       INTEGER,            -- unix seconds; NULL = active
		daily_budget_usd REAL NOT NULL DEFAULT 0
	);
	CREATE UNIQUE INDEX api_keys_active_name ON api_keys(name) WHERE revoked_at IS NULL;`,
}

// KeyStore manages and authenticates access keys. Safe for concurrent use.
type KeyStore struct {
	db   *sql.DB
	path string
}

// OpenKeyStore opens (creating if needed) the SQLite database at path and
// brings its schema up to date. The file is made readable by its owner only.
func OpenKeyStore(path string) (*KeyStore, error) {
	if path == "" {
		return nil, errors.New("auth: database path is required")
	}
	_, statErr := os.Stat(path)
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	s := &KeyStore{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("auth: opening %s: %w", path, err)
	}
	if os.IsNotExist(statErr) {
		// New database: restrict it (WAL/SHM files inherit these permissions).
		if err := os.Chmod(path, 0o600); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

// Close closes the database.
func (s *KeyStore) Close() error { return s.db.Close() }

// Path is the database file.
func (s *KeyStore) Path() string { return s.path }

func (s *KeyStore) migrate() error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this program (v%d)", v, len(migrations))
	}
	for ; v < len(migrations); v++ {
		if _, err := tx.Exec(migrations[v]); err != nil {
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
		return err
	}
	return tx.Commit()
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return b
}

// Create issues a new key and returns its secret, which is not stored
// anywhere and cannot be recovered.
func (s *KeyStore) Create(name string, dailyBudgetUSD float64) (secret string, k Key, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Key{}, errors.New("auth: key name is required")
	}
	if dailyBudgetUSD < 0 {
		return "", Key{}, errors.New("auth: budget must be >= 0")
	}
	secret = keyPrefix + keyEnc.EncodeToString(randomBytes(32))
	k = Key{
		ID: "k_" + hex.EncodeToString(randomBytes(4)), Name: name,
		Created: time.Now().UTC().Truncate(time.Second), DailyBudgetUSD: dailyBudgetUSD,
	}
	_, err = s.db.Exec(`INSERT INTO api_keys (id, name, hash, created_at, daily_budget_usd) VALUES (?, ?, ?, ?, ?)`,
		k.ID, k.Name, hashSecret(secret), k.Created.Unix(), k.DailyBudgetUSD)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && strings.Contains(err.Error(), "name") {
			return "", Key{}, fmt.Errorf("auth: an active key named %q already exists", name)
		}
		return "", Key{}, err
	}
	return secret, k, nil
}

// List returns all keys, including revoked ones, oldest first.
func (s *KeyStore) List() ([]Key, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at, revoked_at, daily_budget_usd FROM api_keys ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Revoke revokes the active key with the given ID or name.
func (s *KeyStore) Revoke(idOrName string) (Key, error) {
	now := time.Now().UTC().Truncate(time.Second)
	row := s.db.QueryRow(`UPDATE api_keys SET revoked_at = ?
		WHERE revoked_at IS NULL AND (id = ? OR name = ?)
		RETURNING id, name, created_at, revoked_at, daily_budget_usd`, now.Unix(), idOrName, idOrName)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, fmt.Errorf("auth: no active key %q", idOrName)
	}
	return k, err
}

// ActiveKeys is the number of usable keys.
func (s *KeyStore) ActiveKeys() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM api_keys WHERE revoked_at IS NULL`).Scan(&n)
	return n, err
}

// Authenticate implements Authenticator.
func (s *KeyStore) Authenticate(r *http.Request) (*Principal, error) {
	tok, ok := BearerToken(r)
	if !ok {
		return nil, ErrNoCredentials
	}
	if !strings.HasPrefix(tok, keyPrefix) {
		return nil, ErrInvalid
	}
	row := s.db.QueryRowContext(r.Context(), `SELECT id, name, created_at, revoked_at, daily_budget_usd
		FROM api_keys WHERE hash = ? AND revoked_at IS NULL`, hashSecret(tok))
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("auth: key lookup: %w", err)
	}
	return k.Principal(), nil
}

type scanner interface{ Scan(dest ...any) error }

func scanKey(sc scanner) (Key, error) {
	var k Key
	var created int64
	var revoked sql.NullInt64
	if err := sc.Scan(&k.ID, &k.Name, &created, &revoked, &k.DailyBudgetUSD); err != nil {
		return Key{}, err
	}
	k.Created = time.Unix(created, 0).UTC()
	if revoked.Valid {
		t := time.Unix(revoked.Int64, 0).UTC()
		k.Revoked = &t
	}
	return k, nil
}
