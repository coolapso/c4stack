package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
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

func (s *Subscriptions) reviewsCommand(ctx context.Context, args []string) error {
	reviews, err := s.Reviews(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tEVENT\tSUBSCRIPTION\tACCOUNT\tREASON\tCREATED")
	for _, r := range reviews {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.EventID, orDash(r.SubscriptionID), orDash(r.AccountID),
			r.Reason, r.CreatedAt.UTC().Format(time.DateTime))
	}
	return w.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (s *Subscriptions) resolveCommand(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: subscriptions resolve <review-id> <note…>")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return ErrNoReview
	}
	if err := s.Resolve(ctx, id, strings.Join(args[1:], " ")); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Resolved review %d.\n", id)
	return nil
}

func (s *Subscriptions) syncCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: subscriptions sync <subscription-id>")
	}
	out, err := s.Sync(ctx, args[0])
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotAtProvider) {
		return errors.New("no subscription with that ID is known to the provider")
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Subscription %s is %s.\n", args[0], out.Status)
	if out.Review != "" {
		fmt.Fprintf(os.Stdout, "It needs a look: %s.\n", out.Review)
	}
	return nil
}
