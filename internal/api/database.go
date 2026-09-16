package api

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ECS injects the password separately from the RDS-managed secret. Never
// concatenate it into a URL without escaping or log the resulting DSN.
func databaseURL() (string, error) {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn, nil
	}
	if os.Getenv("DB_HOST") == "" {
		return "", nil
	}
	if os.Getenv("DB_USER") == "" || os.Getenv("DB_PASSWORD") == "" {
		return "", errors.New("DB_USER and DB_PASSWORD are required")
	}
	u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(os.Getenv("DB_HOST"), env("DB_PORT", "5432")), Path: "/" + env("DB_NAME", "broto"), User: url.UserPassword(os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD"))}
	q := url.Values{"sslmode": {env("DB_SSLMODE", "verify-full")}}
	if root := os.Getenv("DB_SSLROOTCERT"); root != "" {
		q.Set("sslrootcert", root)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func openDatabase(ctx context.Context, dsn string, max int32) (*pgxpool.Pool, error) {
	pc, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		return nil, errors.New("invalid database configuration")
	}
	pc.MaxConns = max
	pc.MinConns = 0
	pc.MaxConnLifetime = 30 * time.Minute
	pc.MaxConnLifetimeJitter = 5 * time.Minute
	pc.MaxConnIdleTime = 5 * time.Minute
	pc.ConnConfig.ConnectTimeout = 10 * time.Second
	pc.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pc.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "180000"
	return pgxpool.NewWithConfig(ctx, pc)
}

// MigrateDatabase is used only by the separate, privileged deployment task.
// Runtime containers use AUTO_MIGRATE=false and a role without DDL privileges.
func MigrateDatabase(ctx context.Context) error {
	dsn, e := databaseURL()
	if e != nil {
		return e
	}
	if dsn == "" {
		return errors.New("database configuration missing")
	}
	db, e := openDatabase(ctx, dsn, 2)
	if e != nil {
		return e
	}
	defer db.Close()
	s := &Server{DB: db}
	if e = s.migrate(ctx); e != nil {
		return e
	}
	if password := os.Getenv("APP_DB_PASSWORD"); password != "" {
		return provisionRuntimeRole(ctx, db, password)
	}
	return nil
}

func provisionRuntimeRole(ctx context.Context, db *pgxpool.Pool, password string) error {
	if len(password) < 32 || strings.ContainsRune(password, 0) {
		return errors.New("APP_DB_PASSWORD must contain at least 32 characters")
	}
	tx, e := db.Begin(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	if _, e = tx.Exec(ctx, "set local standard_conforming_strings=on; select pg_advisory_xact_lock(730210)"); e != nil {
		return e
	}
	var exists bool
	if e = tx.QueryRow(ctx, "select exists(select 1 from pg_roles where rolname='broto_app')").Scan(&exists); e != nil {
		return e
	}
	if !exists {
		if _, e = tx.Exec(ctx, "create role broto_app login nosuperuser nocreatedb nocreaterole noreplication nobypassrls"); e != nil {
			return e
		}
	}
	// SQL DDL cannot bind password parameters. Escape as a SQL string literal;
	// neither this statement nor provider error details are logged by the CLI.
	if _, e = tx.Exec(ctx, "alter role broto_app password '"+strings.ReplaceAll(password, "'", "''")+"'"); e != nil {
		return e
	}
	var name string
	if e = tx.QueryRow(ctx, "select current_database()").Scan(&name); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "revoke create on schema public from public; grant usage on schema public to broto_app; grant select,insert,update,delete on all tables in schema public to broto_app; grant usage,select on all sequences in schema public to broto_app; grant execute on all functions in schema public to broto_app; grant connect on database "+pgx.Identifier{name}.Sanitize()+" to broto_app"); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (s *Server) checkMigrations(ctx context.Context) error {
	files, _ := assets.ReadDir("migrations")
	for _, f := range files {
		var exists bool
		if e := s.DB.QueryRow(ctx, "select exists(select 1 from schema_migrations where name=$1)", f.Name()).Scan(&exists); e != nil || !exists {
			return errors.New("database migrations are pending; run the migration task first")
		}
	}
	return nil
}
