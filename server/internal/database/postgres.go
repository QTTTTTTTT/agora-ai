package database

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	_ "github.com/lib/pq"
)

// Config holds all PostgreSQL connection parameters and pool settings.
type Config struct {
	Host            string
	Port            int
	User            string
	Password        string
	DBName          string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// NewConfig reads database configuration from environment variables,
// falling back to sensible defaults for local development.
func NewConfig() Config {
	cfg := Config{
		Host:            envOrDefault("DB_HOST", "localhost"),
		Port:            envOrDefaultInt("DB_PORT", 5432),
		User:            envOrDefault("DB_USER", "fundai"),
		Password:        envOrDefault("DB_PASSWORD", "fundai_secret"),
		DBName:          envOrDefault("DB_NAME", "fundai"),
		SSLMode:         envOrDefault("DB_SSL_MODE", "disable"),
		MaxOpenConns:    envOrDefaultInt("DB_MAX_OPEN_CONNS", 25),
		MaxIdleConns:    envOrDefaultInt("DB_MAX_IDLE_CONNS", 5),
		ConnMaxLifetime: envOrDefaultDuration("DB_CONN_MAX_LIFETIME", 5*time.Minute),
	}
	return cfg
}

// DSN builds a PostgreSQL connection string from the config.
func (c Config) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, c.Port, c.User, c.Password, c.DBName, c.SSLMode,
	)
}

// Connect opens a PostgreSQL connection, verifies it with a ping,
// and configures the connection pool. Returns the ready-to-use *sql.DB.
func Connect(cfg Config) (*sql.DB, error) {
	db, err := sql.Open("postgres", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("database: failed to open connection: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database: failed to ping after open: %w", err)
	}

	return db, nil
}

// RunMigrations reads all .sql files from migrationsDir in lexicographic
// order and executes each one inside a transaction. Files should be named
// with a numeric prefix to guarantee ordering (e.g. 001_init.sql).
func RunMigrations(db *sql.DB, migrationsDir string) error {
	entries, err := ioutil.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("database: failed to read migrations directory %q: %w", migrationsDir, err)
	}

	// Collect only .sql files.
	var sqlFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) == ".sql" {
			sqlFiles = append(sqlFiles, e.Name())
		}
	}
	sort.Strings(sqlFiles)

	for _, name := range sqlFiles {
		path := filepath.Join(migrationsDir, name)
		content, err := ioutil.ReadFile(path)
		if err != nil {
			return fmt.Errorf("database: failed to read migration file %q: %w", name, err)
		}

		if err := executeMigration(db, name, string(content)); err != nil {
			return err
		}
	}

	return nil
}

// executeMigration runs a single migration file inside a transaction.
func executeMigration(db *sql.DB, name, query string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("database: begin tx for migration %q: %w", name, err)
	}

	if _, err := tx.Exec(query); err != nil {
		tx.Rollback()
		return fmt.Errorf("database: exec migration %q: %w", name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: commit migration %q: %w", name, err)
	}

	return nil
}

// HealthCheck performs a simple ping against the database to verify
// the connection is still alive. Suitable for liveness/readiness probes.
func HealthCheck(db *sql.DB) error {
	if err := db.Ping(); err != nil {
		return fmt.Errorf("database: health check failed: %w", err)
	}
	return nil
}

// TxFunc is the signature for a function executed within a transaction.
type TxFunc func(tx *sql.Tx) error

// WithTransaction starts a transaction, executes fn, and commits on
// success or rolls back on error (including panics). The caller never
// needs to manage Commit/Rollback manually.
func WithTransaction(db *sql.DB, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("database: begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p) // re-raise after rollback
		}
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				err = fmt.Errorf("database: rollback failed (%v) after error: %w", rbErr, err)
			}
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("database: commit transaction: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func envOrDefault(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envOrDefaultInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envOrDefaultDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
