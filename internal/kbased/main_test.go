package kbased

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/kbclient"
	"github.com/bio4554/lab/internal/migrate"
)

func testDSN() string {
	if dsn := os.Getenv("LAB_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://lab:lab@localhost:5432/lab?sslmode=disable"
}

var migrateOnce sync.Once

// testPool connects to the dev Postgres and ensures the kbase stream
// is migrated, skipping the test when the database is unreachable.
// Override the DSN with LAB_TEST_DSN.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Fatal(err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable (run `make db-up`, or set LAB_TEST_DSN): %v", err)
	}
	t.Cleanup(pool.Close)

	migrateOnce.Do(func() {
		db, err := sql.Open("pgx", testDSN())
		if err != nil {
			t.Fatalf("open for migrate: %v", err)
		}
		defer db.Close()
		if err := migrate.Kbase.Up(ctx, db); err != nil {
			t.Fatalf("migrate kbase up: %v", err)
		}
	})

	return pool
}

const testAdminToken = "kbased-test-admin-token"

// fixture is one live kbased over httptest: the store, the HTTP
// server, an admin client, and cleanup that removes every row the test
// created (principals it registered and their entries/versions/
// tokens). The append-only trigger is disabled just for the cleanup
// deletes.
type fixture struct {
	pool  *pgxpool.Pool
	store *Store
	srv   *httptest.Server
	admin *kbclient.Client

	mu         sync.Mutex
	principals []uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testPool(t)
	store := NewStore(pool)
	server := &Server{Store: store, AdminToken: testAdminToken}
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)

	f := &fixture{
		pool:  pool,
		store: store,
		srv:   srv,
		admin: kbclient.New(srv.URL, testAdminToken),
	}
	t.Cleanup(func() { f.cleanup(t) })
	return f
}

func (f *fixture) cleanup(t *testing.T) {
	f.mu.Lock()
	ids := f.principals
	f.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Errorf("cleanup begin: %v", err)
		return
	}
	defer tx.Rollback(ctx)
	stmts := []string{
		"ALTER TABLE kbase.entry_versions DISABLE TRIGGER entry_versions_append_only",
		`DELETE FROM kbase.entry_versions
		 WHERE author = ANY($1)
		    OR entry_id IN (SELECT id FROM kbase.entries WHERE created_by = ANY($1))`,
		"ALTER TABLE kbase.entry_versions ENABLE TRIGGER entry_versions_append_only",
		"DELETE FROM kbase.entries WHERE created_by = ANY($1)",
		"DELETE FROM kbase.tokens WHERE principal_id = ANY($1)",
		"DELETE FROM kbase.principals WHERE id = ANY($1)",
	}
	for _, q := range stmts {
		var err error
		if q[0] == 'A' {
			_, err = tx.Exec(ctx, q)
		} else {
			_, err = tx.Exec(ctx, q, ids)
		}
		if err != nil {
			t.Errorf("cleanup %q: %v", q, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("cleanup commit: %v", err)
	}
}

// principal registers a principal through the admin API with a
// test-unique external id and tracks it for cleanup.
func (f *fixture) principal(t *testing.T, kind, displayName string) kbclient.Principal {
	t.Helper()
	p, err := f.admin.RegisterPrincipal(context.Background(), kbclient.RegisterPrincipalRequest{
		Kind:        kind,
		ExternalID:  t.Name() + "-" + uuid.NewString(),
		DisplayName: displayName,
	})
	if err != nil {
		t.Fatalf("register principal: %v", err)
	}
	f.mu.Lock()
	f.principals = append(f.principals, p.ID)
	f.mu.Unlock()
	return p
}

// token mints a token through the admin API and returns its plaintext.
func (f *fixture) token(t *testing.T, principalID uuid.UUID, projectID *uuid.UUID) string {
	t.Helper()
	resp, err := f.admin.MintToken(context.Background(), kbclient.MintTokenRequest{
		PrincipalID: principalID,
		ProjectID:   projectID,
	})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return resp.Token
}

// client returns an API client authenticating with token.
func (f *fixture) client(token string) *kbclient.Client {
	return kbclient.New(f.srv.URL, token)
}
