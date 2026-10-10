// Package expiry expires credit that has gone unused, after warning its owner.
//
// An account's credit expires after Policy.InactiveMonths without a purchase, debit or
// adjustment. A warning goes out NoticeDays before that, and only a successfully
// delivered warning starts the notice: credit expires once the full notice period from
// delivery has passed, never earlier than announced. New activity starts over, and a
// hold the app reports defers expiry. Accounts without a verified email address are
// never warned, so their credit never expires.
//
// Expiry runs hourly while the app serves, and on demand with the credits expire
// command. A PostgreSQL lock keeps replicas from running it twice at once.
package expiry

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/credits"
	"github.com/coolapso/c4stack/email"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations create the expiry table. List them after credits.Migrations.
var Migrations = app.Migrations{Namespace: "credits_expiry", Files: sub(migrationFiles, "migrations")}

func sub(fsys fs.FS, dir string) fs.FS {
	s, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return s
}

// Policy sets when credit expires. Both values are required.
type Policy struct {
	// InactiveMonths without activity make credit expire, in calendar months.
	InactiveMonths int
	// NoticeDays is the warning before expiry. It runs from the warning's delivery.
	NoticeDays int
}

// Notice is what a warning tells its recipient.
type Notice struct {
	To           string
	AppName      string
	BalanceCents int64
	LastActivity time.Time
	ExpiresAt    time.Time
	CreditsURL   string
}

// Config configures expiry.
type Config struct {
	// Sender delivers the warnings. It is required: credit never expires unwarned.
	Sender email.Sender
	Policy Policy
	// AppName names the app in the default warning, such as "Acme".
	AppName string
	// CreditsURL is the absolute URL of the credits page, linked from the warning.
	CreditsURL string
	// Message writes the warning. Zero uses a plain default that names AppName.
	Message func(Notice) email.Message
}

// Expiry is the credit expiry module.
type Expiry struct {
	app     *app.App
	credits *credits.Credits
	cfg     Config
	now     func() time.Time
}

// New returns the expiry module. It runs expiry hourly while the app serves, and
// registers the credits expire command.
func New(a *app.App, cr *credits.Credits, cfg Config) (*Expiry, error) {
	for _, m := range []app.Migrations{credits.Migrations, Migrations} {
		if err := a.Require(m); err != nil {
			return nil, err
		}
	}
	switch {
	case cfg.Sender == nil:
		return nil, errors.New("expiry: an email sender is required; credit must never expire unwarned")
	case cfg.Policy.InactiveMonths < 1 || cfg.Policy.InactiveMonths > 120:
		return nil, errors.New("expiry: InactiveMonths must be between 1 and 120")
	case cfg.Policy.NoticeDays < 1 || cfg.Policy.NoticeDays > 365:
		return nil, errors.New("expiry: NoticeDays must be between 1 and 365")
	case cfg.AppName == "":
		return nil, errors.New("expiry: AppName is required")
	}
	if u, err := url.Parse(cfg.CreditsURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("expiry: CreditsURL must be an absolute URL")
	}
	if cfg.Message == nil {
		cfg.Message = defaultMessage
	}
	x := &Expiry{app: a, credits: cr, cfg: cfg, now: time.Now}
	a.Every("credit expiry", time.Hour, func(ctx context.Context) error {
		r, err := x.Run(ctx)
		if err == nil && !r.Skipped && r != (Result{}) {
			a.Log().Info("credit expiry ran", "warned", r.Warned, "expired", r.Expired, "failed", r.Failed)
		}
		return err
	})
	a.Command(app.Command{
		Name: "credits expire",
		Help: "Send due expiry warnings and expire credit now, instead of waiting for the hourly run.",
		Run: func(ctx context.Context, _ []string) error {
			r, err := x.Run(ctx)
			if err != nil {
				return err
			}
			if r.Skipped {
				fmt.Fprintln(os.Stdout, "Another expiry run is in progress.")
				return nil
			}
			fmt.Fprintf(os.Stdout, "Warned %d, expired %d, failed %d.\n", r.Warned, r.Expired, r.Failed)
			return nil
		},
	})
	return x, nil
}

// Result counts what one run did.
type Result struct {
	// Skipped means another run held the lock.
	Skipped bool
	Warned  int
	Expired int
	// Failed counts warnings that could not be delivered or accounts that could not be
	// processed. They are retried later.
	Failed int
}

// lockKey identifies the advisory lock that keeps runs from overlapping.
const lockKey = 7_400_215_934_118_201_332

// Run sends due warnings and expires due credit once.
func (x *Expiry) Run(ctx context.Context) (Result, error) {
	conn, err := x.app.DB().Conn(ctx)
	if err != nil {
		return Result{}, err
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, int64(lockKey)).Scan(&locked); err != nil {
		return Result{}, err
	}
	if !locked {
		return Result{Skipped: true}, nil
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(lockKey))
	}()

	now := x.now()
	var r Result
	after := ""
	for {
		batch, err := x.candidates(ctx, now, after)
		if err != nil {
			return r, err
		}
		for _, c := range batch {
			if err := x.process(ctx, now, c, &r); err != nil {
				r.Failed++
				x.app.Log().Warn("credit expiry failed for an account", "account_id", c.accountID, "error", err)
			}
		}
		if len(batch) < batchSize {
			return r, nil
		}
		after = batch[len(batch)-1].accountID
	}
}

const batchSize = 100

// candidate is an account whose credit is due for a warning or expiry.
type candidate struct {
	accountID    string
	balance      int64
	lastActivity time.Time
	nominal      time.Time // when the credit expires without regard to the notice
	email        sql.NullString
	activityAt   sql.NullTime // the notice row's fields, if there is one
	attemptedAt  sql.NullTime
	warnedAt     sql.NullTime
	expiresAt    sql.NullTime
}

func (x *Expiry) candidates(ctx context.Context, now time.Time, after string) ([]candidate, error) {
	rows, err := x.app.DB().QueryContext(ctx, `
		WITH credit AS (
		    SELECT account_id,
		           sum(amount_cents) AS balance,
		           max(created_at) FILTER (WHERE kind IN ('purchase', 'debit', 'adjustment')) AS last_activity
		    FROM credit_ledger
		    WHERE account_id IS NOT NULL
		    GROUP BY account_id
		)
		SELECT c.account_id, c.balance, c.last_activity,
		       c.last_activity + make_interval(months => $1) AS nominal,
		       a.email, n.activity_at, n.attempted_at, n.warned_at, n.expires_at
		FROM credit c
		JOIN accounts a ON a.id = c.account_id
		LEFT JOIN credit_expiry_notices n ON n.account_id = c.account_id
		WHERE c.balance > 0
		    AND c.last_activity + make_interval(months => $1) - make_interval(days => $2) <= $3
		    AND c.account_id::text > $4
		ORDER BY c.account_id::text
		LIMIT $5`,
		x.cfg.Policy.InactiveMonths, x.cfg.Policy.NoticeDays, now, after, batchSize,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.accountID, &c.balance, &c.lastActivity, &c.nominal, &c.email,
			&c.activityAt, &c.attemptedAt, &c.warnedAt, &c.expiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// process warns or expires one account.
func (x *Expiry) process(ctx context.Context, now time.Time, c candidate, r *Result) error {
	notice := time.Duration(x.cfg.Policy.NoticeDays) * 24 * time.Hour
	current := c.activityAt.Valid && c.activityAt.Time.Equal(c.lastActivity)
	if current && c.warnedAt.Valid {
		// A warning announced so long ago that its deadline passed a notice period ago,
		// such as after the app was stopped for months, is renewed rather than acted on.
		stale := now.After(c.expiresAt.Time.Add(notice))
		if !stale {
			due := !now.Before(c.expiresAt.Time) && !now.Before(c.nominal) && !now.Before(c.warnedAt.Time.Add(notice))
			if !due {
				return nil
			}
			expired, err := x.expire(ctx, c)
			if expired {
				r.Expired++
			}
			return err
		}
	}
	if !c.email.Valid {
		return nil // no verified address to warn, so the credit does not expire
	}
	if current && !c.warnedAt.Valid && c.attemptedAt.Valid && now.Sub(c.attemptedAt.Time) < 24*time.Hour {
		return nil // a recent attempt failed; try again tomorrow
	}
	return x.warn(ctx, now, notice, c, r)
}

// warn records the attempt, sends the warning outside any transaction, and records
// the delivery. A failed delivery is retried a day later.
func (x *Expiry) warn(ctx context.Context, now time.Time, notice time.Duration, c candidate, r *Result) error {
	if _, err := x.app.DB().ExecContext(ctx, `
		INSERT INTO credit_expiry_notices (account_id, activity_at, attempted_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (account_id) DO UPDATE
		SET activity_at = excluded.activity_at,
		    attempted_at = excluded.attempted_at,
		    warned_at = NULL,
		    expires_at = NULL`,
		c.accountID, c.lastActivity, now,
	); err != nil {
		return err
	}
	deadline := c.nominal
	if earliest := now.Add(notice); deadline.Before(earliest) {
		deadline = earliest
	}
	err := x.cfg.Sender.Send(ctx, x.cfg.Message(Notice{
		To:           c.email.String,
		AppName:      x.cfg.AppName,
		BalanceCents: c.balance,
		LastActivity: c.lastActivity,
		ExpiresAt:    deadline,
		CreditsURL:   x.cfg.CreditsURL,
	}))
	if err != nil {
		return fmt.Errorf("deliver warning: %w", err)
	}
	if _, err := x.app.DB().ExecContext(ctx, `
		UPDATE credit_expiry_notices
		SET warned_at = $3, expires_at = $4
		WHERE account_id = $1 AND activity_at = $2`,
		c.accountID, c.lastActivity, x.now(), deadline,
	); err != nil {
		return err
	}
	r.Warned++
	return nil
}

// expire removes the account's balance under its lock, after checking that nothing
// changed: no new activity, and no holds.
func (x *Expiry) expire(ctx context.Context, c candidate) (bool, error) {
	expired := false
	err := x.credits.WithAccount(ctx, c.accountID, func(tx *credits.Tx) error {
		var last sql.NullTime
		if err := tx.SQL().QueryRowContext(ctx, `
			SELECT max(created_at)
			FROM credit_ledger
			WHERE account_id = $1 AND kind IN ('purchase', 'debit', 'adjustment')`, c.accountID,
		).Scan(&last); err != nil {
			return err
		}
		if !last.Valid || !last.Time.Equal(c.lastActivity) {
			return nil // used meanwhile; the next run starts over
		}
		balance, err := tx.Balance()
		if err != nil {
			return err
		}
		avail, err := tx.Available()
		if err != nil {
			return err
		}
		if balance <= 0 || avail < balance {
			return nil // nothing left, or held for work in progress
		}
		if err := tx.Expire(balance, "expiry:"+strconv.FormatInt(c.lastActivity.UnixMicro(), 10)); err != nil {
			return err
		}
		if _, err := tx.SQL().ExecContext(ctx, `
			DELETE FROM credit_expiry_notices
			WHERE account_id = $1`, c.accountID,
		); err != nil {
			return err
		}
		expired = true
		return nil
	})
	return expired, err
}

func defaultMessage(n Notice) email.Message {
	day := n.ExpiresAt.UTC().Format("2 January 2006")
	return email.Message{
		To:      n.To,
		Subject: fmt.Sprintf("Your %s credit expires on %s", n.AppName, day),
		Text: fmt.Sprintf(`Hello,

You have %s of %s credit that has not been used since %s.
Unless you use it or buy more credit before %s, it expires on that day.

See your credit: %s
`, credits.FormatEuros(n.BalanceCents), n.AppName, n.LastActivity.UTC().Format("2 January 2006"), day, n.CreditsURL),
	}
}
