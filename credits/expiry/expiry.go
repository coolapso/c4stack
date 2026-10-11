// Package expiry expires credit that has gone unused, after warning its owner.
//
// An account's credit expires after Policy.InactiveMonths without a purchase, charge or
// adjustment. A warning goes out NoticeDays before that, and counts only once the email
// provider reports it delivered: credit expires once a full notice period from delivery
// has passed, and never before the date the warning announced. Each warning's deadline
// is stored, so a later policy change never shortens it. New activity starts over, and
// a hold the app reports defers expiry.
//
// Warnings go only to an account's primary address while one of its identities verifies
// it. Credit never expires while no identity does, and a new primary address needs a
// new warning.
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

// Migrations create the warnings table. List them after credits.Migrations.
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
	// Sender sends the warnings and reports their delivery. It is required: credit
	// never expires without a delivered warning.
	Sender email.TrackingSender
	Policy Policy
	// AppName names the app in the default warning, such as "Acme".
	AppName string
	// CreditsURL is the absolute URL of the credits page, linked from the warning.
	CreditsURL string
	// Message writes the warning. Zero uses a plain default that names AppName. The
	// warning always goes to Notice.To, whatever the message's To says.
	Message func(Notice) email.Message
	// OnRun, if set, receives the outcome of every run: the hourly ones, the credits
	// expire command's and the app's own calls to Run. Use it to record metrics. It
	// runs after the run released its lock, and must return quickly.
	OnRun func(ctx context.Context, r Result, err error)
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
			a.Log().Info("credit expiry ran", "warned", r.Warned, "warnings_failed", r.WarningsFailed,
				"expired", r.Expired, "expired_cents", r.ExpiredCents, "failed", r.Failed)
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
			fmt.Fprintf(os.Stdout, "Warned %d, %d warnings failed. Expired %s from %d accounts. %d other failures.\n",
				r.Warned, r.WarningsFailed, credits.FormatEuros(r.ExpiredCents), r.Expired, r.Failed)
			return nil
		},
	})
	return x, nil
}

// Result counts what one run did. The next run tries again whatever failed.
type Result struct {
	// Skipped means another run held the lock, so this one did nothing.
	Skipped bool
	// Warned counts warnings the email provider accepted, and WarningsFailed those it
	// did not.
	Warned         int
	WarningsFailed int
	// Expired counts accounts whose credit expired, and ExpiredCents that credit.
	Expired      int
	ExpiredCents int64
	// Failed counts everything else that failed: checking a warning's delivery,
	// recording a warning that was sent, and expiring an account's credit.
	Failed int
}

// lockKey identifies the advisory lock that keeps runs from overlapping, across
// replicas and deployments that share the database.
const lockKey = 739_084_311_982_417_602

// Run checks the delivery of sent warnings, sends due warnings and expires due credit,
// once, and reports the outcome to Config.OnRun.
func (x *Expiry) Run(ctx context.Context) (Result, error) {
	r, err := x.run(ctx)
	if x.cfg.OnRun != nil {
		x.cfg.OnRun(ctx, r, err)
	}
	return r, err
}

func (x *Expiry) run(ctx context.Context) (Result, error) {
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
	if err := x.confirmAll(ctx, now, &r); err != nil {
		return r, err
	}
	if err := x.warnAll(ctx, now, &r); err != nil {
		return r, err
	}
	return r, x.expireAll(ctx, now, &r)
}

const batchSize = 100

// resendAfter is how long a warning may stay undelivered before it is sent again.
const resendAfter = 3 * 24 * time.Hour

// activity summarises each account's balance and last activity: its latest purchase,
// charge or adjustment.
const activity = `
	SELECT account_id,
	       sum(amount_cents) AS balance,
	       max(created_at) FILTER (WHERE kind IN ('top_up', 'usage', 'adjustment')) AS last_activity
	FROM credit_ledger
	WHERE account_id IS NOT NULL
	GROUP BY account_id`

// sent is a warning the provider has not reported delivered yet.
type sent struct {
	accountID  string
	messageID  string
	sentAt     time.Time
	noticeDays int
	announced  time.Time
}

// confirmAll asks the provider what became of each undelivered warning. A delivered
// warning starts its notice period at delivery; a failed one, or one undelivered for
// too long, is removed so the next step sends it again.
func (x *Expiry) confirmAll(ctx context.Context, now time.Time, r *Result) error {
	after := ""
	for {
		batch, err := x.undelivered(ctx, after)
		if err != nil {
			return err
		}
		for _, w := range batch {
			if err := x.confirm(ctx, now, w); err != nil {
				r.Failed++
				x.app.Log().Warn("checking a credit expiry warning failed", "account_id", w.accountID, "error", err)
			}
		}
		if len(batch) < batchSize {
			return nil
		}
		after = batch[len(batch)-1].accountID
	}
}

func (x *Expiry) undelivered(ctx context.Context, after string) ([]sent, error) {
	rows, err := x.app.DB().QueryContext(ctx, `
		SELECT account_id, message_id, sent_at, notice_days, announced_expiry
		FROM credit_expiry_warnings
		WHERE delivered_at IS NULL AND message_id IS NOT NULL AND account_id::text > $1
		ORDER BY account_id::text
		LIMIT $2`, after, batchSize,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []sent
	for rows.Next() {
		var w sent
		if err := rows.Scan(&w.accountID, &w.messageID, &w.sentAt, &w.noticeDays, &w.announced); err != nil {
			return nil, err
		}
		batch = append(batch, w)
	}
	return batch, rows.Err()
}

func (x *Expiry) confirm(ctx context.Context, now time.Time, w sent) error {
	state, at, err := x.cfg.Sender.Delivery(ctx, w.messageID)
	if err != nil {
		return err
	}
	switch {
	case state == email.Delivered:
		// The notice period the warning was sent with, so a policy change made since
		// never shortens it.
		expires := at.AddDate(0, 0, w.noticeDays)
		if expires.Before(w.announced) {
			expires = w.announced
		}
		_, err = x.app.DB().ExecContext(ctx, `
			UPDATE credit_expiry_warnings
			SET delivered_at = $3, expires_at = $4
			WHERE account_id = $1 AND message_id = $2`, w.accountID, w.messageID, at, expires,
		)
		return err
	case state == email.Failed || now.Sub(w.sentAt) > resendAfter:
		if state == email.Failed {
			x.app.Log().Warn("a credit expiry warning was not delivered; it is sent again", "account_id", w.accountID)
		}
		_, err = x.app.DB().ExecContext(ctx, `
			DELETE FROM credit_expiry_warnings
			WHERE account_id = $1 AND message_id = $2 AND delivered_at IS NULL`, w.accountID, w.messageID,
		)
		return err
	}
	return nil
}

// verified holds when one of account a's identities verifies its primary address: the
// rule accounts applies before a new identity may join with that address. Warnings go
// only to such an address.
const verified = `EXISTS (
	SELECT 1
	FROM account_identities v
	WHERE v.account_id = a.id AND v.email = a.primary_email AND v.email_verified
)`

// due is an account with credit due for a warning.
type due struct {
	accountID    string
	email        string
	balance      int64
	lastActivity time.Time
	announced    time.Time
}

// warnAll warns every account whose credit expires within the notice period and that
// has no warning about its last activity to its current address.
func (x *Expiry) warnAll(ctx context.Context, now time.Time, r *Result) error {
	after := ""
	for {
		batch, err := x.dueWarnings(ctx, now, after)
		if err != nil {
			return err
		}
		for _, d := range batch {
			sent, err := x.warn(ctx, now, d)
			switch {
			case !sent:
				r.WarningsFailed++
				x.app.Log().Warn("credit expiry warning not sent", "account_id", d.accountID, "error", err)
			case err != nil:
				r.Warned++
				r.Failed++
				x.app.Log().Warn("credit expiry warning sent but not recorded; it is sent again", "account_id", d.accountID, "error", err)
			default:
				r.Warned++
			}
		}
		if len(batch) < batchSize {
			return nil
		}
		after = batch[len(batch)-1].accountID
	}
}

// dueWarnings returns the next batch of accounts due for a warning, after the account ID
// after. The announced date is the nominal expiry, or a full notice period from now if
// that is later.
func (x *Expiry) dueWarnings(ctx context.Context, now time.Time, after string) ([]due, error) {
	rows, err := x.app.DB().QueryContext(ctx, `
		WITH activity AS (`+activity+`)
		SELECT a.id, a.primary_email, c.balance, c.last_activity,
		       GREATEST(
		           c.last_activity + make_interval(months => $1),
		           $3::timestamptz + make_interval(days => $2)
		       )
		FROM activity c
		JOIN accounts a ON a.id = c.account_id
		LEFT JOIN credit_expiry_warnings w ON w.account_id = c.account_id
		WHERE c.balance > 0
		    AND c.last_activity + make_interval(months => $1) - make_interval(days => $2) <= $3
		    AND `+verified+`
		    AND (w.account_id IS NULL OR w.activity_at < c.last_activity OR w.email <> a.primary_email)
		    AND a.id::text > $4
		ORDER BY a.id::text
		LIMIT $5`,
		x.cfg.Policy.InactiveMonths, x.cfg.Policy.NoticeDays, now, after, batchSize,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.accountID, &d.email, &d.balance, &d.lastActivity, &d.announced); err != nil {
			return nil, err
		}
		batch = append(batch, d)
	}
	return batch, rows.Err()
}

// warn sends one warning and records it until the provider reports its delivery. It
// reports whether the provider accepted the warning. If recording fails, the next run
// sends the warning again, which is safer than expiring credit without one.
func (x *Expiry) warn(ctx context.Context, now time.Time, d due) (bool, error) {
	m := x.cfg.Message(Notice{
		To:           d.email,
		AppName:      x.cfg.AppName,
		BalanceCents: d.balance,
		LastActivity: d.lastActivity,
		ExpiresAt:    d.announced,
		CreditsURL:   x.cfg.CreditsURL,
	})
	m.To = d.email
	id, err := x.cfg.Sender.Send(ctx, m)
	if err != nil {
		return false, err
	}
	_, err = x.app.DB().ExecContext(ctx, `
		INSERT INTO credit_expiry_warnings (account_id, activity_at, email, message_id, sent_at, notice_days, announced_expiry)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (account_id) DO UPDATE
		SET activity_at = excluded.activity_at,
		    email = excluded.email,
		    message_id = excluded.message_id,
		    sent_at = excluded.sent_at,
		    notice_days = excluded.notice_days,
		    announced_expiry = excluded.announced_expiry,
		    delivered_at = NULL,
		    expires_at = NULL`,
		d.accountID, d.lastActivity, d.email, id, now, x.cfg.Policy.NoticeDays, d.announced,
	)
	return true, err
}

// expired is the condition for credit to expire: a delivered warning about the
// account's last activity whose deadline has passed, sent to the account's primary
// address, which one of its identities still verifies. $1 is now.
const expired = `
	c.balance > 0
	AND w.activity_at = c.last_activity
	AND w.expires_at <= $1
	AND w.email = a.primary_email
	AND ` + verified

// expireAll expires the credit of every account where it is due.
func (x *Expiry) expireAll(ctx context.Context, now time.Time, r *Result) error {
	after := ""
	for {
		batch, err := x.dueExpiries(ctx, now, after)
		if err != nil {
			return err
		}
		for _, id := range batch {
			cents, err := x.expire(ctx, now, id)
			switch {
			case err != nil:
				r.Failed++
				x.app.Log().Warn("credit expiry failed for an account", "account_id", id, "error", err)
			case cents > 0:
				r.Expired++
				r.ExpiredCents += cents
			}
		}
		if len(batch) < batchSize {
			return nil
		}
		after = batch[len(batch)-1]
	}
}

// dueExpiries returns the next batch of accounts whose credit is due to expire, after
// the account ID after.
func (x *Expiry) dueExpiries(ctx context.Context, now time.Time, after string) ([]string, error) {
	rows, err := x.app.DB().QueryContext(ctx, `
		WITH activity AS (`+activity+`)
		SELECT w.account_id
		FROM credit_expiry_warnings w
		JOIN activity c ON c.account_id = w.account_id
		JOIN accounts a ON a.id = w.account_id
		WHERE `+expired+`
		    AND w.account_id::text > $2
		ORDER BY w.account_id::text
		LIMIT $3`, now, after, batchSize,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		batch = append(batch, id)
	}
	return batch, rows.Err()
}

// expire removes the account's balance under its lock, after checking that the credit
// is still due and that the app holds none of it for work in progress. It returns the
// credit it expired.
func (x *Expiry) expire(ctx context.Context, now time.Time, accountID string) (int64, error) {
	var cents int64
	err := x.credits.WithAccount(ctx, accountID, func(tx *credits.Tx) error {
		var balance int64
		err := tx.SQL().QueryRowContext(ctx, `
			WITH activity AS (`+activity+`)
			SELECT c.balance
			FROM credit_expiry_warnings w
			JOIN activity c ON c.account_id = w.account_id
			JOIN accounts a ON a.id = w.account_id
			WHERE w.account_id = $2 AND `+expired,
			now, accountID,
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
		if _, err := tx.SQL().ExecContext(ctx, `
			DELETE FROM credit_expiry_warnings
			WHERE account_id = $1`, accountID,
		); err != nil {
			return err
		}
		cents = balance
		return nil
	})
	return cents, err
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
