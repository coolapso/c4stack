package subscriptions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/coolapso/c4stack/app"
)

func (s *Subscriptions) registerCommands() {
	s.app.Command(app.Command{
		Name:  "subscriptions list",
		Usage: "<account-id>",
		Help:  "List an account's subscriptions.",
		Run:   s.listCommand,
	})
	s.app.Command(app.Command{
		Name: "subscriptions reviews",
		Help: "List subscription events waiting for an operator.",
		Run:  s.reviewsCommand,
	})
	s.app.Command(app.Command{
		Name:  "subscriptions resolve",
		Usage: "<review-id> <note…>",
		Help:  "Mark a subscription review as handled, with a note on what was done.",
		Run:   s.resolveCommand,
	})
	s.app.Command(app.Command{
		Name:  "subscriptions sync",
		Usage: "<subscription-id>",
		Help:  "Read a subscription's state from the payment provider and apply it, such as after missed webhooks.",
		Run:   s.syncCommand,
	})
}

func (s *Subscriptions) listCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: subscriptions list <account-id>")
	}
	subs, err := s.List(ctx, args[0])
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPRODUCT\tUNITS\tTARGET\tSTATUS\tPERIOD END\tTHEN")
	for _, sub := range subs {
		end := "-"
		if !sub.PeriodEnd.IsZero() {
			end = sub.PeriodEnd.UTC().Format(time.DateTime)
		}
		then := "-"
		if sub.Pending != nil {
			then = fmt.Sprintf("%s × %d", sub.Pending.Product.ID, sub.Pending.Units)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", sub.ID, sub.Product.ID, sub.Units, sub.Target, sub.Status, end, then)
	}
	return w.Flush()
}

func (s *Subscriptions) reviewsCommand(ctx context.Context, args []string) (err error) {
	rows, err := s.app.DB().QueryContext(ctx, `
		SELECT id, event_id, COALESCE(subscription_id::text, '-'), reason, created_at
		FROM subscription_reviews
		WHERE resolved_at IS NULL
		ORDER BY created_at`,
	)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tEVENT\tSUBSCRIPTION\tREASON\tCREATED")
	for rows.Next() {
		var id int64
		var event, sub, reason string
		var created time.Time
		if err := rows.Scan(&id, &event, &sub, &reason, &created); err != nil {
			return err
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", id, event, sub, reason, created.UTC().Format(time.DateTime))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return w.Flush()
}

func (s *Subscriptions) resolveCommand(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: subscriptions resolve <review-id> <note…>")
	}
	res, err := s.app.DB().ExecContext(ctx, `
		UPDATE subscription_reviews
		SET resolved_at = now(), resolution = $2
		WHERE id::text = $1 AND resolved_at IS NULL`, args[0], strings.Join(args[1:], " "),
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no open review has that ID")
	}
	fmt.Fprintf(os.Stdout, "Resolved review %s.\n", args[0])
	return nil
}

func (s *Subscriptions) syncCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: subscriptions sync <subscription-id>")
	}
	var providerID sql.NullString
	err := s.app.DB().QueryRowContext(ctx, `
		SELECT provider_subscription_id
		FROM subscriptions
		WHERE id::text = $1`, args[0],
	).Scan(&providerID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !providerID.Valid {
		return errors.New("no subscription with that ID is known to the provider")
	}
	if err != nil {
		return err
	}
	state, err := s.payments.Subscription(ctx, providerID.String)
	if err != nil {
		return err
	}
	var out applied
	if err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.applyState(ctx, tx, args[0], state, time.Time{})
		return err
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Subscription %s is %s.\n", args[0], state.Status)
	if out.review != "" {
		fmt.Fprintf(os.Stdout, "It needs a look: %s.\n", out.review)
	}
	return nil
}
