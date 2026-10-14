// Package indexchecker は、テスト中に発行されたクエリに対して
// enable_seqscan = off の状態で EXPLAIN をかけ、「使えるインデックスがない」
// クエリを検出する。データ量に依存しないので、テストデータが少なくても使える。
//
// クエリの収集は ORM / ドライバーごとのアダプタ（bunhook など）が Recorder に対して行う。
// 本パッケージはテストコードからのみ import することを想定している。
package indexchecker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Config は検査の設定。
type Config struct {
	// フルスキャンを許容するテーブル（件数の少ないマスタなど）。
	AllowTables []string
	// 検査を除外するクエリ（正規表現）。意図的なフルスキャンや集計など。
	IgnoreQueries []*regexp.Regexp
}

// Violation は検出された問題 1 件。
type Violation struct {
	Query  string
	Table  string
	Node   string // "Seq Scan" など
	Filter string // インデックスで処理されずに残った条件
}

func (v Violation) String() string {
	return fmt.Sprintf("[%s on %s] Filter: %s\n  query: %s", v.Node, v.Table, v.Filter, v.Query)
}

// Recorder は発行されたクエリを重複なしで記録する。並行して呼び出してよい。
type Recorder struct {
	mu      sync.Mutex
	queries map[string]struct{}
}

func NewRecorder() *Recorder {
	return &Recorder{queries: map[string]struct{}{}}
}

// Add はクエリを記録する。EXPLAIN できない文（INSERT など）は無視する。
func (r *Recorder) Add(query string) {
	if !Explainable(query) {
		return
	}
	r.mu.Lock()
	r.queries[query] = struct{}{}
	r.mu.Unlock()
}

// Queries は記録したクエリを返す（ソート済み）。
func (r *Recorder) Queries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	qs := make([]string, 0, len(r.queries))
	for q := range r.queries {
		qs = append(qs, q)
	}
	sort.Strings(qs)
	return qs
}

// Check は記録したクエリをすべて検査する。
// db には、クエリ収集用のフックを登録していない接続を渡す。
func (r *Recorder) Check(ctx context.Context, db *sql.DB, cfg Config) ([]Violation, error) {
	return Check(ctx, db, r.Queries(), cfg)
}

var explainable = regexp.MustCompile(`(?is)^\s*(with\b.*?\b)?(select|update|delete)\b`)

// Explainable は検査対象の文（SELECT / UPDATE / DELETE）かどうかを返す。
func Explainable(query string) bool {
	q := strings.TrimSpace(query)
	if strings.HasPrefix(strings.ToUpper(q), "EXPLAIN") {
		return false
	}
	return explainable.MatchString(q)
}

// Check は渡されたクエリをすべて検査する。
// クエリにはプレースホルダーではなく値が埋め込まれている必要がある。
func Check(ctx context.Context, db *sql.DB, queries []string, cfg Config) ([]Violation, error) {
	allow := map[string]bool{}
	for _, t := range cfg.AllowTables {
		allow[t] = true
	}

	var all []Violation
	for _, q := range queries {
		if ignored(q, cfg.IgnoreQueries) {
			continue
		}
		vs, err := checkQuery(ctx, db, q, allow)
		if err != nil {
			return nil, fmt.Errorf("explain failed: %w\n  query: %s", err, q)
		}
		all = append(all, vs...)
	}
	return all, nil
}

func ignored(q string, patterns []*regexp.Regexp) bool {
	for _, p := range patterns {
		if p.MatchString(q) {
			return true
		}
	}
	return false
}

// plan は EXPLAIN (FORMAT JSON) のノード。必要な項目だけ持つ。
type plan struct {
	NodeType     string `json:"Node Type"`
	RelationName string `json:"Relation Name"`
	IndexCond    string `json:"Index Cond"`
	Filter       string `json:"Filter"`
	Plans        []plan `json:"Plans"`
}

func checkQuery(ctx context.Context, db *sql.DB, query string, allow map[string]bool) ([]Violation, error) {
	// トランザクション内で設定し、最後にロールバックする。
	// EXPLAIN（ANALYZE なし）はクエリを実行しないので、UPDATE/DELETE でもデータは変わらない。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		return nil, err
	}

	var raw []byte
	if err := tx.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON) "+query).Scan(&raw); err != nil {
		return nil, err
	}

	var out []struct {
		Plan plan `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}

	var vs []Violation
	for _, o := range out {
		walk(o.Plan, func(p plan) {
			if p.RelationName == "" || allow[p.RelationName] || p.Filter == "" {
				return
			}
			switch p.NodeType {
			case "Seq Scan":
				// Seq Scan を禁止してもなお Seq Scan → 使えるインデックスがない
			case "Index Scan", "Index Only Scan":
				// インデックスは使っているが、条件はすべて Filter で処理されている
				// （主キーなどを先頭から全部なめているだけ）→ 実質フルスキャン
				if p.IndexCond != "" {
					return
				}
			default:
				return
			}
			vs = append(vs, Violation{
				Query:  query,
				Table:  p.RelationName,
				Node:   p.NodeType,
				Filter: p.Filter,
			})
		})
	}
	return vs, nil
}

func walk(p plan, fn func(plan)) {
	fn(p)
	for _, c := range p.Plans {
		walk(c, fn)
	}
}
