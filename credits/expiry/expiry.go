// Package expiry expires credit that has gone unused, after warning its owner.
//
// An account's credit expires after Policy.InactiveMonths without a purchase, charge or
// adjustment. A warning goes out NoticeDays before that, and only a delivered warning
// counts: credit expires once a full notice period from delivery has passed, never
// earlier than announced. New activity starts over, and a hold the app reports defers
// expiry. Shortening NoticeDays also shortens warnings already sent, so do not shorten
// it while warnings are outstanding.
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
	"time"

	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/credits"
	"github.com/coolapso/c4stack/email"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations add the warning column to accounts. List them after credits.Migrations.
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
	// processed. The next run tries them again.
	Failed int
}

// lockKey identifies the advisory lock that keeps runs from overlapping, across
// replicas and deployments that share the database.
const lockKey = 739_084_311_982_417_602

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

	now := x.now().UTC()
	var r Result
	if err := x.warnAll(ctx, now, &r); err != nil {
		return r, err
	}
	return r, x.expireAll(ctx, now, &r)
}

const batchSize = 100

// activity summarises each account's balance and last activity: its latest purchase,
// charge or adjustment.
const activity = `
	SELECT account_id,
	       sum(amount_cents) AS balance,
	       max(created_at) FILTER (WHERE kind IN ('top_up', 'usage', 'adjustment')) AS last_activity
	FROM credit_ledger
	WHERE account_id IS NOT NULL
	GROUP BY account_id`

// due is an account with credit due for a warning.
type due struct {
	accountID    string
	email        string
	balance      int64
	lastActivity time.Time
	expiresAt    time.Time
}

// warnAll warns every account whose credit expires within the notice period and that
// has not been warned since its last activity. The credit expires no sooner than a
// full notice period after the warning.
func (x *Expiry) warnAll(ctx context.Context, now time.Time, r *Result) error {
	after := ""
	for {
		rows, err := x.app.DB().QueryContext(ctx, `
			WITH activity AS (`+activity+`)
			SELECT a.id, a.primary_email, c.balance, c.last_activity,
			       GREATEST(
			           c.last_activity + make_interval(months => $1),
			           $3::timestamptz + make_interval(days => $2)
			       )
			FROM activity c
			JOIN accounts a ON a.id = c.account_id
			WHERE c.balance > 0
			    AND c.last_activity + make_interval(months => $1) - make_interval(days => $2) <= $3
			    AND (a.credit_expiry_warned_at IS NULL OR a.credit_expiry_warned_at < c.last_activity)
			    AND a.id::text > $4
			ORDER BY a.id::text
			LIMIT $5`,
			x.cfg.Policy.InactiveMonths, x.cfg.Policy.NoticeDays, now, after, batchSize,
		)
		if err != nil {
			return err
		}
		var batch []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.accountID, &d.email, &d.balance, &d.lastActivity, &d.expiresAt); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range batch {
			if err := x.warn(ctx, now, d); err != nil {
				r.Failed++
				x.app.Log().Warn("credit expiry warning failed", "account_id", d.accountID, "error", err)
				continue
			}
			r.Warned++
		}
		if len(batch) < batchSize {
			return nil
		}
		after = batch[len(batch)-1].accountID
	}
}

// warn sends one warning and records its delivery. If recording fails, the next run
// sends the warning again, which is safer than expiring credit without one.
func (x *Expiry) warn(ctx context.Context, now time.Time, d due) error {
	err := x.cfg.Sender.Send(ctx, x.cfg.Message(Notice{
		To:           d.email,
		AppName:      x.cfg.AppName,
		BalanceCents: d.balance,
		LastActivity: d.lastActivity,
		ExpiresAt:    d.expiresAt,
		CreditsURL:   x.cfg.CreditsURL,
	}))
	if err != nil {
		return fmt.Errorf("deliver warning: %w", err)
	}
	_, err = x.app.DB().ExecContext(ctx, `
		UPDATE accounts
		SET credit_expiry_warned_at = $2
		WHERE id = $1`, d.accountID, now,
	)
	return err
}

// expired is the condition for credit to expire, on the activity and accounts rows:
// InactiveMonths without activity, and a warning sent since that activity at least
// NoticeDays ago. $1 is InactiveMonths, $2 NoticeDays and $3 now.
const expired = `
	c.balance > 0
	AND c.last_activity + make_interval(months => $1) <= $3
	AND a.credit_expiry_warned_at IS NOT NULL
	AND a.credit_expiry_warned_at >= c.last_activity
	AND a.credit_expiry_warned_at + make_interval(days => $2) <= $3`

// expireAll expires the credit of every account where it is due.
func (x *Expiry) expireAll(ctx context.Context, now time.Time, r *Result) error {
	after := ""
	for {
		rows, err := x.app.DB().QueryContext(ctx, `
			WITH activity AS (`+activity+`)
			SELECT a.id
			FROM activity c
			JOIN accounts a ON a.id = c.account_id
			WHERE `+expired+`
			    AND a.id::text > $4
			ORDER BY a.id::text
			LIMIT $5`,
			x.cfg.Policy.InactiveMonths, x.cfg.Policy.NoticeDays, now, after, batchSize,
		)
		if err != nil {
			return err
		}
		var batch []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range batch {
			done, err := x.expire(ctx, now, id)
			switch {
			case err != nil:
				r.Failed++
				x.app.Log().Warn("credit expiry failed for an account", "account_id", id, "error", err)
			case done:
				r.Expired++
			}
		}
		if len(batch) < batchSize {
			return nil
		}
		after = batch[len(batch)-1]
	}
}

// expire removes the account's balance under its lock, after checking that the credit
// is still due and that the app holds none of it for work in progress.
func (x *Expiry) expire(ctx context.Context, now time.Time, accountID string) (bool, error) {
	done := false
	err := x.credits.WithAccount(ctx, accountID, func(tx *credits.Tx) error {
		var balance int64
		err := tx.SQL().QueryRowContext(ctx, `
			WITH activity AS (`+activity+`)
			SELECT c.balance
			FROM activity c
			JOIN accounts a ON a.id = c.account_id
			WHERE a.id = $4 AND `+expired,
			x.cfg.Policy.InactiveMonths, x.cfg.Policy.NoticeDays, now, accountID,
		).Scan(&balance)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // used or emptied meanwhile
		}
		if err != nil {
			return err
		}
		avail, err := tx.Available()
		if err != nil || avail < balance {
			return err // held for work in progress: the next run tries again
		}
		if err := tx.Expire(balance); err != nil {
			return err
		}
		done = true
		return nil
	})
	return done, err
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
