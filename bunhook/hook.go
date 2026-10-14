// Package bunhook は、Bun が発行したクエリを indexchecker.Recorder に記録する
// bun.QueryHook を提供する。
package bunhook

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	indexchecker "github.com/endlfu/index-check"
)

// Hook は bun.DB.AddQueryHook に登録して使う。
type Hook struct {
	rec *indexchecker.Recorder
}

var _ bun.QueryHook = (*Hook)(nil)

func New(rec *indexchecker.Recorder) *Hook {
	return &Hook{rec: rec}
}

func (h *Hook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *Hook) AfterQuery(_ context.Context, e *bun.QueryEvent) {
	// 失敗したクエリは検査しても意味がない（該当なしは正常なので検査する）
	if e.Err != nil && !errors.Is(e.Err, sql.ErrNoRows) {
		return
	}
	h.rec.Add(e.Query)
}
