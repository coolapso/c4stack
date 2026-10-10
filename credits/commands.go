package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/coolapso/c4stack/app"
)

func (cr *Credits) registerCommands() {
	cr.app.Command(app.Command{
		Name:  "credits adjust",
		Usage: "<account-id> <cents> <reference> <description…>",
		Help:  "Add or remove credit, such as a correction or a restored dispute. Negative cents remove.",
		Run:   cr.adjustCommand,
	})
	cr.app.Command(app.Command{
		Name:  "credits balance",
		Usage: "<account-id>",
		Help:  "Show an account's balance, holds and available credit.",
		Run:   cr.balanceCommand,
	})
	cr.app.Command(app.Command{
		Name: "credits reviews",
		Help: "List payment events waiting for an operator.",
		Run:  cr.reviewsCommand,
	})
	cr.app.Command(app.Command{
		Name:  "credits resolve",
		Usage: "<review-id> <note…>",
		Help:  "Mark a payment review as handled, with a note on what was done.",
		Run:   cr.resolveCommand,
	})
}

func (cr *Credits) adjustCommand(ctx context.Context, args []string) error {
	if len(args) < 4 {
		return errors.New("usage: credits adjust <account-id> <cents> <reference> <description…>")
	}
	cents, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return errors.New("cents must be a whole number, such as 500 or -500")
	}
	if err := cr.Adjust(ctx, args[0], cents, args[2], strings.Join(args[3:], " ")); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Adjusted account %s by %s.\n", args[0], FormatEuros(cents))
	return nil
}

func (cr *Credits) balanceCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: credits balance <account-id>")
	}
	var exists bool
	if err := cr.app.DB().QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM accounts WHERE id::text = $1)`, args[0],
	).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	s, err := cr.Summary(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Balance:   %s\nHeld:      %s\nAvailable: %s\n",
		FormatEuros(s.BalanceCents), FormatEuros(s.HeldCents), FormatEuros(s.AvailableCents))
	return nil
}

func (cr *Credits) reviewsCommand(ctx context.Context, args []string) error {
	rows, err := cr.app.DB().QueryContext(ctx, `
		SELECT r.id, r.created_at, r.provider, r.event_id, r.reason, r.intent_id, i.account_id
		FROM payment_reviews r
		LEFT JOIN payment_intents i ON i.id = r.intent_id
		WHERE r.resolved_at IS NULL
		ORDER BY r.created_at`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tCREATED\tPROVIDER\tEVENT\tREASON\tINTENT\tACCOUNT")
	n := 0
	for rows.Next() {
		var id int64
		var created time.Time
		var provider, event, reason string
		var intentID, accountID sql.NullString
		if err := rows.Scan(&id, &created, &provider, &event, &reason, &intentID, &accountID); err != nil {
			return err
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", id, created.UTC().Format(time.DateTime), provider, event, reason,
			orDash(intentID), orDash(accountID))
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n == 0 {
		fmt.Fprintln(os.Stdout, "No open reviews.")
		return nil
	}
	return w.Flush()
}

func orDash(s sql.NullString) string {
	if !s.Valid {
		return "-"
	}
	return s.String
}

func (cr *Credits) resolveCommand(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: credits resolve <review-id> <note…>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return errors.New("the review ID is a number; see credits reviews")
	}
	res, err := cr.app.DB().ExecContext(ctx, `
		UPDATE payment_reviews
		SET resolved_at = now(), resolution = $2
		WHERE id = $1 AND resolved_at IS NULL`, id, strings.Join(args[1:], " "),
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no open review %d", id)
	}
	fmt.Fprintf(os.Stdout, "Resolved review %d.\n", id)
	return nil
}
