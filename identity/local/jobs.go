package local

import (
	"context"
	"time"
)

// The advisory locks that keep replicas from running a job twice at once.
const (
	noticesLockKey = 739_084_311_982_417_611
	cleanupLockKey = 739_084_311_982_417_612
)

func (l *Local) registerJobs() {
	l.app.Every("sign-in notices", time.Minute, func(ctx context.Context) error {
		return l.withLock(ctx, noticesLockKey, l.sendNotices)
	})
	l.app.Every("sign-in cleanup", time.Hour, func(ctx context.Context) error {
		return l.withLock(ctx, cleanupLockKey, l.cleanup)
	})
}

// withLock runs fn while holding an advisory lock, and does nothing when another run
// holds it.
func (l *Local) withLock(ctx context.Context, key int64, fn func(context.Context) error) error {
	conn, err := l.app.DB().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, key)
	}()
	return fn(ctx)
}

// cleanup removes rows that no longer count: expired codes, ended sessions and
// windows, and answered challenges past their expiry. It never decides whether a limit
// applies: the counters reset their own windows.
func (l *Local) cleanup(ctx context.Context) error {
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`
			DELETE FROM local_codes
			WHERE expires_at < now()`, nil},
		{`
			DELETE FROM local_limits
			WHERE window_ends_at < now()`, nil},
		{`
			DELETE FROM local_used_challenges
			WHERE expires_at < now()`, nil},
		{`
			DELETE FROM local_sessions
			WHERE created_at < now() - make_interval(secs => $1)
			    OR last_seen_at < now() - make_interval(secs => $2)`,
			[]any{sessionLifetime.Seconds(), sessionIdle.Seconds()}},
	} {
		if _, err := l.app.DB().ExecContext(ctx, q.sql, q.args...); err != nil {
			return err
		}
	}
	return nil
}
