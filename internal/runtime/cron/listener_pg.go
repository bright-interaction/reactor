package cron

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PgListener subscribes to the reactor_triggers_changed channel on a
// Postgres database and invokes onNotify whenever a row in the triggers
// table changes. Drops a fresh Reconcile call into the cron driver so
// runtime trigger CRUD takes effect within milliseconds instead of
// waiting for the polling reconcile.
//
// Lives next to the polling reloadLoop; both run on Postgres so a
// dropped notification (the channel is at-most-once delivery, never
// queued) still gets picked up by the next poll. The polling interval
// can be cranked higher on Postgres deployments since LISTEN is the
// primary path.
type PgListener struct {
	DSN string
	Log *slog.Logger
}

// Listen blocks until ctx is cancelled. Reconnects on transient
// connection drops with exponential backoff (capped at 30s).
func (l *PgListener) Listen(ctx context.Context, onNotify func(payload string)) error {
	if l.Log == nil {
		l.Log = slog.Default()
	}
	defer func() {
		if r := recover(); r != nil {
			l.Log.Error("cron: pg listener panicked",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := l.listenOnce(ctx, onNotify); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			l.Log.Warn("cron: pg listener disconnected; reconnecting",
				"err", err, "backoff", backoff.String())
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		// Clean return = ctx cancelled inside listenOnce.
		return nil
	}
}

// listenOnce opens a single pgx.Conn, subscribes, and loops on
// WaitForNotification until the conn drops or ctx cancels.
func (l *PgListener) listenOnce(ctx context.Context, onNotify func(payload string)) error {
	dsn := normaliseDSN(l.DSN)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "LISTEN reactor_triggers_changed"); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	l.Log.Info("cron: pg listener subscribed", "channel", "reactor_triggers_changed")

	for {
		notif, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait: %w", err)
		}
		if onNotify != nil {
			onNotify(notif.Payload)
		}
	}
}

// normaliseDSN strips the postgres:// scheme prefix variants that pgx's
// stdlib bridge accepts but pgx.Connect parses more strictly. Idempotent.
func normaliseDSN(dsn string) string {
	// pgx.Connect accepts both URL form and keyword/value form; the
	// app's existing REACTOR_DB_URL is always URL form. Strip the "pgx://"
	// alias if present.
	switch {
	case strings.HasPrefix(dsn, "pgx://"):
		return "postgres://" + strings.TrimPrefix(dsn, "pgx://")
	default:
		return dsn
	}
}
