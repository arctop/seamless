package console

import (
	"context"
	"errors"
	"log/slog"
)

// clientGoneHandler demotes a console log record that carries a
// context.Canceled error to debug. A console request's context is cancelled
// only when the browser stops waiting -- a click away mid-load, a closed tab, a
// refresh -- so the query that failed with it failed because nobody wanted the
// answer, not because anything is wrong. Logged as found, one abandoned project
// page wrote an ERROR "request failed" plus a WARN from each best-effort helper
// (nav counts, features, level): 12 ERRORs and 33 WARNs on the live log, every
// one "context canceled", on pages that render in a fifth of a second. A
// deadline (context.DeadlineExceeded) is a slow query and keeps its level.
type clientGoneHandler struct{ slog.Handler }

func (h clientGoneHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level <= slog.LevelDebug || !carriesCanceled(r) {
		return h.Handler.Handle(ctx, r)
	}
	if !h.Enabled(ctx, slog.LevelDebug) {
		return nil
	}
	demoted := slog.NewRecord(r.Time, slog.LevelDebug, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		demoted.AddAttrs(a)
		return true
	})
	return h.Handler.Handle(ctx, demoted)
}

func (h clientGoneHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return clientGoneHandler{h.Handler.WithAttrs(attrs)}
}

func (h clientGoneHandler) WithGroup(name string) slog.Handler {
	return clientGoneHandler{h.Handler.WithGroup(name)}
}

// carriesCanceled reports whether any of the record's attributes is an error
// wrapping context.Canceled.
func carriesCanceled(r slog.Record) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if err, ok := a.Value.Any().(error); ok && errors.Is(err, context.Canceled) {
			found = true
		}
		return !found
	})
	return found
}
