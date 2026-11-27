package marketplace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/coolapso/c4stack/apikeys"
	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/credits"
)

func (m *Marketplace) registerCommands() {
	m.app.Command(app.Command{
		Name:  "marketplace grants",
		Usage: "[unclaimed|claimed|ended|test]",
		Help:  "List marketplace grants, newest first: each buyer's account, credit and state.",
		Run:   m.grantsCommand,
	})
	m.app.Command(app.Command{
		Name:  "marketplace grant",
		Usage: "<grant-id|order-id|account-id|key-suffix>",
		Help:  "Show a marketplace grant and every request answered about it.",
		Run:   m.grantCommand,
	})
	m.app.Command(app.Command{
		Name:  "marketplace claim-link",
		Usage: "<grant-id>",
		Help:  "Replace an unclaimed grant's claim link, for an owner who lost it and proved the purchase, and print the new one.",
		Run:   m.claimLinkCommand,
	})
	m.app.Command(app.Command{
		Name:  "marketplace revoke",
		Usage: "<grant-id>",
		Help:  "Stop a grant's API key and refuse its top-ups, such as after abuse. Its credit stays.",
		Run:   m.revokeCommand,
	})
	m.app.Command(app.Command{
		Name: "marketplace remove-tests",
		Help: "Delete the accounts the provider's listing reviews opened.",
		Run:  m.removeTestsCommand,
	})
	if m.provider != nil {
		m.app.Command(app.Command{
			Name: "marketplace run",
			Help: "Wipe sealed credentials past their deadline and expire inactive unclaimed accounts now.",
			Run: func(ctx context.Context, _ []string) error {
				r, err := m.Run(ctx)
				if err != nil {
					return err
				}
				if r.Skipped {
					fmt.Fprintln(os.Stdout, "Another run is in progress.")
					return nil
				}
				fmt.Fprintf(os.Stdout, "Wiped %d sealed credentials. Expired %s from %d unclaimed accounts. %d failed.\n",
					r.SecretsWiped, credits.FormatEuros(r.ExpiredCents), r.Expired, r.Failed)
				return nil
			},
		})
	}
}

// state names a grant's state for operators.
const state = `CASE
	WHEN g.test THEN 'test'
	WHEN g.ended_at IS NOT NULL THEN 'ended'
	WHEN g.revoked_at IS NOT NULL THEN 'revoked'
	WHEN g.claimed_at IS NOT NULL THEN 'claimed'
	ELSE 'unclaimed'
END`

func (m *Marketplace) grantsCommand(ctx context.Context, args []string) error {
	filter := ""
	if len(args) > 1 {
		return errors.New("usage: marketplace grants [unclaimed|claimed|ended|test]")
	}
	if len(args) == 1 {
		switch args[0] {
		case "unclaimed", "claimed", "ended", "test":
			filter = args[0]
		default:
			return errors.New("usage: marketplace grants [unclaimed|claimed|ended|test]")
		}
	}
	rows, err := m.app.DB().QueryContext(ctx, `
		SELECT g.grant_id, `+state+`, g.account_id,
		       COALESCE((SELECT sum(amount_cents) FROM credit_ledger l WHERE l.account_id = g.account_id), 0),
		       CASE WHEN g.account_id IS NULL THEN NULL ELSE `+lastActivity+` END,
		       g.created_at
		FROM marketplace_grants g
		WHERE $1 = '' OR `+state+` = $1 OR ($1 = 'ended' AND `+state+` = 'revoked')
		ORDER BY g.created_at DESC
		LIMIT 500`, filter,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "GRANT\tSTATE\tACCOUNT\tBALANCE\tLAST ACTIVITY\tCREATED")
	n := 0
	for rows.Next() {
		var grantID, st string
		var accountID sql.NullString
		var balance int64
		var last sql.NullTime
		var created time.Time
		if err := rows.Scan(&grantID, &st, &accountID, &balance, &last, &created); err != nil {
			return err
		}
		lastText := "-"
		if last.Valid {
			lastText = last.Time.UTC().Format(time.DateTime)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", grantID, st, orDash(accountID), credits.FormatEuros(balance), lastText,
			created.UTC().Format(time.DateTime))
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n == 0 {
		fmt.Fprintln(os.Stdout, "No grants.")
		return nil
	}
	return w.Flush()
}

func (m *Marketplace) grantCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: marketplace grant <grant-id|order-id|account-id|key-suffix>")
	}
	var ref, grantID, st, provider string
	var accountID, keySuffix, orderID sql.NullString
	var created time.Time
	var claimed, revoked, ended sql.NullTime
	err := m.app.DB().QueryRowContext(ctx, `
		SELECT g.id, g.provider, g.grant_id, `+state+`, g.account_id, k.key_suffix, g.order_id,
		       g.created_at, g.claimed_at, g.revoked_at, g.ended_at
		FROM marketplace_grants g
		LEFT JOIN api_keys k ON k.id = g.key_id
		WHERE g.grant_id = $1 OR g.account_id::text = $1 OR k.key_suffix = $1
		    OR EXISTS (SELECT 1 FROM marketplace_operations o WHERE o.grant_ref = g.id AND o.order_id = $1)
		ORDER BY g.created_at DESC
		LIMIT 1`, args[0],
	).Scan(&ref, &provider, &grantID, &st, &accountID, &keySuffix, &orderID, &created, &claimed, &revoked, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no grant matches %q", args[0])
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Grant:     %s (%s)\nState:     %s\nAccount:   %s\nKey:       %s\nProvision: %s\nCreated:   %s\n",
		grantID, provider, st, orDash(accountID), orDash(keySuffix), orDash(orderID), created.UTC().Format(time.DateTime))
	for _, t := range []struct {
		label string
		at    sql.NullTime
	}{{"Claimed", claimed}, {"Revoked", revoked}, {"Ended", ended}} {
		if t.at.Valid {
			fmt.Fprintf(os.Stdout, "%-10s %s\n", t.label+":", t.at.Time.UTC().Format(time.DateTime))
		}
	}
	if accountID.Valid {
		s, err := m.credits.Summary(ctx, accountID.String)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "Balance:   %s\n", credits.FormatEuros(s.BalanceCents))
	}
	rows, err := m.app.DB().QueryContext(ctx, `
		SELECT created_at, action, status, COALESCE(order_id, ''), event_id, operation_id, test
		FROM marketplace_operations
		WHERE grant_ref = $1
		ORDER BY created_at`, ref,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Fprintln(os.Stdout)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ANSWERED\tACTION\tSTATUS\tORDER\tEVENT\tOPERATION\tTEST")
	for rows.Next() {
		var at time.Time
		var action, status, order, event, operation string
		var test bool
		if err := rows.Scan(&at, &action, &status, &order, &event, &operation, &test); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%t\n", at.UTC().Format(time.DateTime), action, status, order, event, operation, test)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return w.Flush()
}

func (m *Marketplace) claimLinkCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: marketplace claim-link <grant-id>")
	}
	link, err := m.NewClaimLink(ctx, args[0])
	if errors.Is(err, ErrClaimInvalid) {
		return fmt.Errorf("grant %s has no account that can be claimed", args[0])
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "New claim link (the old one no longer works):\n%s\n", link)
	return nil
}

func (m *Marketplace) revokeCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: marketplace revoke <grant-id>")
	}
	tx, err := m.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var g grant
	err = tx.QueryRowContext(ctx, `
		SELECT id, account_id, key_id
		FROM marketplace_grants
		WHERE grant_id = $1 AND revoked_at IS NULL AND ended_at IS NULL
		FOR UPDATE`, args[0],
	).Scan(&g.id, &g.accountID, &g.keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no active grant %s", args[0])
	}
	if err != nil {
		return err
	}
	if g.accountID.Valid && g.keyID.Valid {
		if err := m.keys.RevokeTx(ctx, tx, g.accountID.String, g.keyID.String); err != nil && !errors.Is(err, apikeys.ErrNotFound) {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE marketplace_grants
		SET revoked_at = now(), claim_token_hash = NULL
		WHERE id = $1`, g.id,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Revoked grant %s: its key stopped working and top-ups are refused.\n", args[0])
	return nil
}

func (m *Marketplace) removeTestsCommand(ctx context.Context, _ []string) error {
	grants, err := m.testGrants(ctx)
	if err != nil {
		return err
	}
	for _, g := range grants {
		tx, err := m.app.DB().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := m.end(ctx, tx, g); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stdout, "Removed %d test accounts.\n", len(grants))
	return nil
}

// testGrants returns the grants of the provider's listing reviews whose accounts remain.
func (m *Marketplace) testGrants(ctx context.Context) ([]grant, error) {
	rows, err := m.app.DB().QueryContext(ctx, `
		SELECT id, account_id
		FROM marketplace_grants
		WHERE test AND ended_at IS NULL AND account_id IS NOT NULL AND claimed_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []grant
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.id, &g.accountID); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

func orDash(s sql.NullString) string {
	if !s.Valid {
		return "-"
	}
	return s.String
}
