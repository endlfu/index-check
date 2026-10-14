# index-check

テスト中に発行されたクエリに対して、PostgreSQL の `enable_seqscan = off` の状態で `EXPLAIN` をかけ、**使えるインデックスがないクエリ**を検出します。

プランナーの判断ではなく「インデックスが存在するか」を見るので、テストデータが 1 件しかなくても判定できます。CI でインデックスの貼り忘れを止める用途を想定しています。

## 判定ルール

| プラン | 判定 |
| --- | --- |
| Seq Scan を禁止しても、条件付きの `Seq Scan` が残る | 違反（使えるインデックスがない） |
| `Index Scan` だが `Index Cond` がなく、条件がすべて `Filter` で処理されている | 違反（主キーなどを全部なめているだけ） |
| 条件の一部でも `Index Cond` に入っている | 合格 |

## 使い方（Bun）

テストコードからのみ import してください。本番のバイナリには含まれません。

```go
package app

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	indexchecker "github.com/endlfu/index-check"
	"github.com/endlfu/index-check/bunhook"
)

var (
	testDB   *bun.DB
	recorder = indexchecker.NewRecorder()
)

func TestMain(m *testing.M) {
	sqldb, _ := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	testDB = bun.NewDB(sqldb, pgdialect.New())
	testDB.AddQueryHook(bunhook.New(recorder))

	code := m.Run()

	if code == 0 {
		vs, err := recorder.Check(context.Background(), sqldb, indexchecker.Config{
			AllowTables:   []string{"prefectures"},                            // 件数の少ないマスタ
			IgnoreQueries: []*regexp.Regexp{regexp.MustCompile(`(?i)count\(`)}, // 意図的な集計
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
		for _, v := range vs {
			fmt.Fprintln(os.Stderr, v)
			code = 1
		}
	}
	os.Exit(code)
}
```

出力例:

```
[Seq Scan on users] Filter: (email = 'a@example.com'::text)
  query: SELECT "user"."id", ... FROM "users" AS "user" WHERE (email = 'a@example.com')
```

## 他のドライバーで使う

`Recorder.Add(query)` に値の埋め込まれたクエリ文字列を渡せば、Bun 以外からも使えます。クエリ一覧がすでにあるなら `indexchecker.Check(ctx, db, queries, cfg)` を直接呼べます。

## 制限

- テストで実行されたクエリしか検査できません。網羅性はテストのカバレッジ次第です。
- JOIN の結合キー（`Hash Cond` など）は検査しません。
- 複合インデックスの一部しか使われていない場合は合格扱いです。
- 対象は `SELECT` / `UPDATE` / `DELETE` です。`EXPLAIN` は `ANALYZE` なしで実行するため、データは変更されません。

## 開発

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable go test ./...
```

`TEST_DATABASE_URL` がない場合、DB を使うテストはスキップされます。
