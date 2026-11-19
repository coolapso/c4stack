package local

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/netip"
	"time"

	"github.com/gin-gonic/gin"
)

// A limit is a fixed number of events in a fixed window, which starts at the window's
// first event. docs/sign-in.md ("The limits") says what each one prevents.
type limit struct {
	name   string
	max    int
	window time.Duration
}

var (
	// Codes sent to one address, through its shared counters: flooding a mailbox.
	sendAddressHour = limit{"send-address-hour", 5, time.Hour}
	sendAddressDay  = limit{"send-address-day", 10, 24 * time.Hour}
	// Codes a browser that signed in before asks for its user's address: the same
	// values, so someone else using up the shared counters doesn't stop the owner.
	sendDeviceHour = limit{"send-device-hour", 5, time.Hour}
	sendDeviceDay  = limit{"send-device-day", 10, 24 * time.Hour}
	// Codes asked for from one network: one machine making the app email many
	// addresses.
	sendNetworkHour = limit{"send-network-hour", 30, time.Hour}
	// Wrong codes for one address: guessing across many codes. At the limit, the
	// shared counters stop codes for the address until the window ends.
	wrongAddressDay = limit{"wrong-address-day", 20, 24 * time.Hour}
	wrongDeviceDay  = limit{"wrong-device-day", 20, 24 * time.Hour}
	// Wrong codes from one network: guessing across many addresses.
	wrongNetworkHour = limit{"wrong-network-hour", 100, time.Hour}
	// Codes a signed-in user asks for to change their address: an account flooding a
	// stranger's mailbox.
	newAddressUserHour = limit{"new-address-user-hour", 5, time.Hour}
	newAddressUserDay  = limit{"new-address-user-day", 10, 24 * time.Hour}
)

// sendAppHour is the app's optional ceiling on the codes it sends.
func (l *Local) sendAppHour() limit {
	return limit{"send-app-hour", l.cfg.MaxCodesPerHour, time.Hour}
}

// A counter is a limit applied to one thing: an address, a network, a known browser, a
// user, or the app.
type counter struct {
	limit
	subject string
}

func (l *Local) counterKey(c counter) []byte {
	mac := hmac.New(sha256.New, l.limitKey)
	mac.Write([]byte(c.name))
	mac.Write([]byte{0})
	mac.Write([]byte(c.subject))
	return mac.Sum(nil)
}

// limitedError means a limit stopped the request, until a window ends.
type limitedError struct {
	until time.Time
}

func (e *limitedError) Error() string {
	return fmt.Sprintf("local: limited until %s", e.until.UTC().Format(time.RFC3339))
}

// count adds one event to each counter, in the order given, and returns a limitedError
// for the first that goes over its limit. The caller rolls the transaction back then, so
// a refused request counts nowhere.
func (l *Local) count(ctx context.Context, tx *sql.Tx, counters ...counter) error {
	for _, c := range counters {
		n, ends, err := l.bump(ctx, tx, c, 1)
		if err != nil {
			return err
		}
		if n > c.max {
			return &limitedError{until: ends}
		}
	}
	return nil
}

// check locks each counter, in the order given, without counting, and returns a
// limitedError for the first that is at its limit. The locks make concurrent entries
// for the same counters wait, so add counts exactly.
func (l *Local) check(ctx context.Context, tx *sql.Tx, counters ...counter) error {
	for _, c := range counters {
		n, ends, err := l.bump(ctx, tx, c, 0)
		if err != nil {
			return err
		}
		if n >= c.max {
			return &limitedError{until: ends}
		}
	}
	return nil
}

// add counts one event on a counter, and returns its count.
func (l *Local) add(ctx context.Context, tx *sql.Tx, c counter) (int, time.Time, error) {
	return l.bump(ctx, tx, c, 1)
}

// bump adds n events to a counter in one upsert, starting a new window when the old one
// has ended, and returns the count and when the window ends. With n = 0 it only locks
// the counter, and an ended window counts as empty.
func (l *Local) bump(ctx context.Context, tx *sql.Tx, c counter, n int) (int, time.Time, error) {
	var count int
	var ends time.Time
	err := tx.QueryRowContext(ctx, `
		INSERT INTO local_limits AS x (key, count, window_ends_at)
		VALUES ($1, $2, CASE WHEN $2 > 0 THEN now() + make_interval(secs => $3) ELSE now() END)
		ON CONFLICT (key) DO UPDATE
		SET count = CASE WHEN x.window_ends_at <= now() THEN $2 ELSE x.count + $2 END,
		    window_ends_at = CASE
		        WHEN x.window_ends_at > now() THEN x.window_ends_at
		        WHEN $2 > 0 THEN now() + make_interval(secs => $3)
		        ELSE now()
		    END
		RETURNING count, window_ends_at`,
		l.counterKey(c), n, c.window.Seconds(),
	).Scan(&count, &ends)
	return count, ends, err
}

// network is what the per-network limits count: an IPv4 address, or an IPv6 /64, from
// the client IP as web.Config.TrustedProxies makes it.
func network(c *gin.Context) string {
	ip, err := netip.ParseAddr(c.ClientIP())
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	if ip.Is4() {
		return ip.String()
	}
	p, err := ip.Prefix(64)
	if err != nil {
		return "unknown"
	}
	return p.String()
}
