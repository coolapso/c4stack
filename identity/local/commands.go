package local

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/coolapso/c4stack/app"
	"github.com/coolapso/c4stack/identity"
	"github.com/jackc/pgx/v5/pgconn"
)

// errNoUser means an operator command named no user.
var errNoUser = errors.New("local: no user has this ID or address")

func (l *Local) registerCommands() {
	l.app.Command(app.Command{
		Name:  "local sign-out",
		Usage: "<user>",
		Help:  "End every session of a user, given by ID or address, and reset their known browsers.",
		Run:   l.signOutCommand,
	})
	l.app.Command(app.Command{
		Name:  "local unblock",
		Usage: "<address>",
		Help:  "Reset an address's shared sign-in limits, so codes work for it again at once.",
		Run:   l.unblockCommand,
	})
	l.app.Command(app.Command{
		Name:  "local set-email",
		Usage: "<user> <new-address>",
		Help:  "Support's way back into an account: set its address, end its sessions, remove its passkeys, and tell both addresses.",
		Run:   l.setEmailCommand,
	})
}

// stdout is where commands print what they did.
var stdout io.Writer = os.Stdout

// findUser locks the user an operator named by ID or current address, inside tx.
func findUser(ctx context.Context, tx *sql.Tx, ref string) (id, address string, err error) {
	query := `
		SELECT id, email
		FROM local_users
		WHERE email = $1
		FOR NO KEY UPDATE`
	arg := ref
	if validUUID(ref) {
		query = `
			SELECT id, email
			FROM local_users
			WHERE id = $1
			FOR NO KEY UPDATE`
	} else if a, ok := normalizeAddress(ref); ok {
		arg = a
	} else {
		return "", "", errNoUser
	}
	err = tx.QueryRowContext(ctx, query, arg).Scan(&id, &address)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errNoUser
	}
	return id, address, err
}

func (l *Local) signOutCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: local sign-out <user>")
	}
	var userID string
	var ended int64
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if userID, _, err = findUser(ctx, tx, args[0]); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			DELETE FROM local_sessions
			WHERE user_id = $1`, userID,
		)
		if err != nil {
			return err
		}
		if ended, err = res.RowsAffected(); err != nil {
			return err
		}
		return resetDevices(ctx, tx, userID)
	})
	if err != nil {
		return err
	}
	l.app.Log().Info("an operator signed a user out everywhere", "user_id", userID, "sessions", ended)
	fmt.Fprintf(stdout, "Ended %d sessions of user %s, and reset their known browsers.\n", ended, userID)
	return nil
}

func (l *Local) unblockCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: local unblock <address>")
	}
	address, ok := normalizeAddress(args[0])
	if !ok {
		return errors.New("local: that is not an email address")
	}
	keys := [][]byte{
		l.counterKey(counter{sendAddressHour, address}),
		l.counterKey(counter{sendAddressDay, address}),
		l.counterKey(counter{wrongAddressDay, address}),
	}
	for _, key := range keys {
		if _, err := l.app.DB().ExecContext(ctx, `
			DELETE FROM local_limits
			WHERE key = $1`, key,
		); err != nil {
			return err
		}
	}
	l.app.Log().Info("an operator reset an address's sign-in limits")
	fmt.Fprintln(stdout, "Reset the address's shared limits. Codes work for it again.")
	return nil
}

func (l *Local) setEmailCommand(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: local set-email <user> <new-address>")
	}
	newAddress, ok := normalizeAddress(args[1])
	if !ok {
		return errors.New("local: the new address is not an email address")
	}
	var userID, oldAddress string
	var notices []string
	err := l.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if userID, oldAddress, err = findUser(ctx, tx, args[0]); err != nil {
			return err
		}
		if oldAddress == newAddress {
			return errors.New("local: the user has this address already")
		}
		if err := l.moveAddress(ctx, tx, userID, newAddress); err != nil {
			return err
		}
		// Nothing an attacker added survives a recovery.
		for _, q := range []string{`
			DELETE FROM local_sessions
			WHERE user_id = $1`, `
			DELETE FROM local_passkeys
			WHERE user_id = $1`,
		} {
			if _, err := tx.ExecContext(ctx, q, userID); err != nil {
				return err
			}
		}
		at := time.Now()
		for _, n := range []struct {
			kind Kind
			to   string
		}{{AddressChangedBySupport, oldAddress}, {AddressSetBySupport, newAddress}} {
			id, err := l.queueNotice(ctx, tx, userID, n.kind, n.to, noticeData{
				OldEmail: oldAddress, NewEmail: newAddress, At: at, Reference: userID,
			})
			if err != nil {
				return err
			}
			notices = append(notices, id)
		}
		return nil
	})
	if errors.Is(err, identity.ErrAddressTaken) {
		return errors.New("local: another user or account has the new address; nothing changed")
	}
	if err != nil {
		return err
	}
	l.app.Log().Info("an operator changed a user's address", "user_id", userID)
	fmt.Fprintf(stdout, "Changed the address of user %s, ended its sessions and removed its passkeys.\n", userID)
	if l.deliver(ctx, notices...) {
		fmt.Fprintln(stdout, "The email provider accepted the notices to the old and the new address.")
	} else {
		fmt.Fprintln(stdout, "Some notices are still queued; the app retries them every minute for 3 days.")
	}
	return nil
}

// moveAddress gives a locked user a new address inside tx: the accounts side first,
// then the user, whose waiting codes go, and whose known browsers are reset. It
// returns identity.ErrAddressTaken if another user or account has the address.
func (l *Local) moveAddress(ctx context.Context, tx *sql.Tx, userID, address string) error {
	if l.hooks.AddressChanged != nil {
		if err := l.hooks.AddressChanged(ctx, tx, userID, address); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE local_users
		SET email = $2, devices_since = now(), updated_at = now()
		WHERE id = $1`, userID, address,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return identity.ErrAddressTaken
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		DELETE FROM local_codes
		WHERE user_id = $1`, userID,
	)
	return err
}

// resetDevices makes every device cookie issued before now stop counting as a known
// browser of the user.
func resetDevices(ctx context.Context, tx *sql.Tx, userID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE local_users
		SET devices_since = now(), updated_at = now()
		WHERE id = $1`, userID,
	)
	return err
}
