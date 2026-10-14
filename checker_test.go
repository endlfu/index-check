package indexchecker_test

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	indexchecker "github.com/endlfu/index-check"
	"github.com/endlfu/index-check/bunhook"
)

func TestExplainable(t *testing.T) {
	cases := map[string]bool{
		"SELECT 1":                               true,
		"  update users set name = 'a'":          true,
		"DELETE FROM users":                      true,
		"WITH x AS (SELECT 1) SELECT * FROM x":   true,
		"INSERT INTO users (email) VALUES ('a')": false,
		"EXPLAIN SELECT 1":                       false,
		"CREATE INDEX i ON users (email)":        false,
	}
	for q, want := range cases {
		if got := indexchecker.Explainable(q); got != want {
			t.Errorf("Explainable(%q) = %v, want %v", q, got, want)
		}
	}
}

type user struct {
	bun.BaseModel `bun:"table:ic_users"`
	ID            int64 `bun:",pk,autoincrement"`
	Email         string
	Name          string
}

// TEST_DATABASE_URL が設定されているときだけ実 DB で検査する。
func TestCheckWithBun(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()

	exec := func(q string) {
		t.Helper()
		if _, err := sqldb.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`DROP TABLE IF EXISTS ic_users`)
	exec(`CREATE TABLE ic_users (id bigserial PRIMARY KEY, email text NOT NULL, name text)`)
	exec(`INSERT INTO ic_users (email, name) VALUES ('a@example.com', 'A')`)
	t.Cleanup(func() { sqldb.ExecContext(ctx, `DROP TABLE IF EXISTS ic_users`) })

	run := func() []indexchecker.Violation {
		t.Helper()
		rec := indexchecker.NewRecorder()
		db := bun.NewDB(sqldb, pgdialect.New())
		db.AddQueryHook(bunhook.New(rec))

		var u user
		if err := db.NewSelect().Model(&u).Where("email = ?", "a@example.com").Scan(ctx); err != nil {
			t.Fatal(err)
		}
		var us []user
		if err := db.NewSelect().Model(&us).Where("name = ?", "A").Scan(ctx); err != nil {
			t.Fatal(err)
		}
		if err := db.NewSelect().Model(&u).WherePK().Scan(ctx); err != nil {
			t.Fatal(err)
		}

		vs, err := rec.Check(ctx, sqldb, indexchecker.Config{
			IgnoreQueries: []*regexp.Regexp{regexp.MustCompile(`name = 'A'`)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return vs
	}

	// email にインデックスがない → 1 件検出（name は除外、主キー検索は問題なし）
	if vs := run(); len(vs) != 1 || vs[0].Table != "ic_users" {
		t.Fatalf("want 1 violation on ic_users, got %v", vs)
	}

	// インデックスを貼れば検出されない
	exec(`CREATE INDEX ic_users_email_idx ON ic_users (email)`)
	if vs := run(); len(vs) != 0 {
		t.Fatalf("want no violations, got %v", vs)
	}
}
