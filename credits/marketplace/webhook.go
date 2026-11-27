package marketplace

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/coolapso/c4stack/accounts"
	"github.com/coolapso/c4stack/apikeys"
	"github.com/coolapso/c4stack/credits"
	"github.com/coolapso/c4stack/payments"
	"github.com/coolapso/c4stack/web"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

// answerTimeout bounds answering one request. Creem waits 15 seconds; an answer that
// isn't ready in time is pending, and the provider asks again.
const answerTimeout = 10 * time.Second

// secretGrace keeps sealed credentials a little past the request's deadline, for a
// repeat the provider sent just before it.
const secretGrace = time.Hour

// Webhook receives the store's payment webhooks. Mount it in place of
// credits.Webhook, on a public route with neither CSRF nor sign-in, such as
// POST /webhooks/payments: it answers the marketplace's grant requests, and hands every
// other event to the credits module. Without a provider it answers 404.
//
// It replies 200 with the answer for every verified request: an answer that couldn't
// be worked out now is pending, which the provider asks again. It logs event, grant,
// operation and order IDs and outcomes, never bodies or credentials.
func (m *Marketplace) Webhook(c *gin.Context) {
	if m.provider == nil {
		c.Status(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusBadRequest)
		return
	}
	log := web.Log(c).With("provider", m.provider.Name())
	req, err := m.provider.ParseGrantRequest(c.Request.Header, body)
	switch {
	case errors.Is(err, payments.ErrIgnored):
		if !m.credits.Purchasing() {
			// The app sells only on the marketplace: other events aren't its business.
			log.Info("payment webhook ignored", "event_id", req.EventID)
			c.Status(http.StatusOK)
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		m.credits.Webhook(c)
		return
	case err != nil:
		log.Warn("marketplace request rejected", "error", err)
		c.Status(http.StatusBadRequest)
		return
	}
	log = log.With("event_id", req.EventID, "grant_id", req.GrantID, "operation_id", req.OperationID,
		"action", string(req.Action), "test", req.Test)
	start := time.Now()
	ctx, cancel := context.WithTimeout(c.Request.Context(), answerTimeout)
	defer cancel()
	answer, repeat, err := m.answer(ctx, req)
	if err != nil {
		log.Error("marketplace request failed; the provider asks again", "error", err)
		answer = payments.GrantAnswer{Status: payments.GrantPending}
	}
	reply, err := m.provider.GrantReply(answer)
	if err != nil {
		log.Error("marketplace answer refused by the provider's format; the provider asks again", "error", err)
		answer = payments.GrantAnswer{Status: payments.GrantPending}
		reply, _ = m.provider.GrantReply(answer)
	}
	c.Data(http.StatusOK, "application/json", reply)
	switch answer.Status {
	case payments.GrantFailed:
		log.Warn("marketplace request refused", "order_id", req.OrderID, "reason", answer.FailureReason, "repeat", repeat)
	case payments.GrantReady:
		log.Info("marketplace request answered", "order_id", req.OrderID, "repeat", repeat)
	}
	if m.cfg.OnAnswer != nil {
		m.cfg.OnAnswer(c.Request.Context(), Answered{Action: req.Action, Status: answer.Status, Repeat: repeat,
			Test: req.Test, Duration: time.Since(start)})
	}
}

// grant is a stored grant, locked by the transaction that read it.
type grant struct {
	id        string
	accountID sql.NullString
	keyID     sql.NullString
	orderID   sql.NullString
	test      bool
	claimed   bool
	revoked   bool
	ended     bool
}

// active holds while the grant's account exists and serves the buyer.
func (g grant) active() bool {
	return g.accountID.Valid && !g.revoked && !g.ended
}

// result is an answer worked out inside the request's transaction.
type result struct {
	answer  payments.GrantAnswer
	grantID string // the grant row, once there is one
	env     map[string]string
	review  string // why an operator should look at it, if they should
}

// answer answers a request once per operation, in one transaction: a repeat gets the
// answer recorded the first time.
func (m *Marketplace) answer(ctx context.Context, req payments.GrantRequest) (payments.GrantAnswer, bool, error) {
	tx, err := m.app.DB().BeginTx(ctx, nil)
	if err != nil {
		return payments.GrantAnswer{}, false, err
	}
	defer tx.Rollback()
	// Deliveries of one operation take turns, so the second finds the first's answer.
	if _, err := tx.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "marketplace:"+m.provider.Name()+":"+req.OperationID,
	); err != nil {
		return payments.GrantAnswer{}, false, err
	}
	if a, found, err := m.recorded(ctx, tx, req.OperationID); found || err != nil {
		return a, true, err
	}
	var r result
	switch req.Action {
	case payments.GrantProvision:
		r, err = m.provision(ctx, tx, req)
	case payments.GrantTopUp:
		r, err = m.topUp(ctx, tx, req)
	case payments.GrantRevoke:
		r, err = m.revoke(ctx, tx, req)
	case payments.GrantRevokeCredit:
		r, err = m.revokeCredit(ctx, tx, req)
	default:
		return payments.GrantAnswer{}, false, errors.New("marketplace: unknown action")
	}
	if err != nil {
		return payments.GrantAnswer{}, false, err
	}
	if err := m.record(ctx, tx, req, r); err != nil {
		return payments.GrantAnswer{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return payments.GrantAnswer{}, false, err
	}
	if r.review != "" {
		// After the commit, so a review never outlives an answer that rolled back. A
		// failure here loses only the note: the logs keep the refusal.
		if err := m.credits.Review(ctx, m.provider.Name(), req.EventID, r.review); err != nil {
			m.app.Log().Warn("recording a marketplace review failed", "event_id", req.EventID, "error", err)
		}
	}
	r.answer.Env = r.env
	return r.answer, false, nil
}

// recorded returns the answer recorded for an operation, with its credentials while
// they are kept.
func (m *Marketplace) recorded(ctx context.Context, tx *sql.Tx, operationID string) (payments.GrantAnswer, bool, error) {
	var stored []byte
	var secret []byte
	err := tx.QueryRowContext(ctx, `
		SELECT answer, secret
		FROM marketplace_operations
		WHERE provider = $1 AND operation_id = $2`, m.provider.Name(), operationID,
	).Scan(&stored, &secret)
	if errors.Is(err, sql.ErrNoRows) {
		return payments.GrantAnswer{}, false, nil
	}
	if err != nil {
		return payments.GrantAnswer{}, false, err
	}
	var a payments.GrantAnswer
	if err := json.Unmarshal(stored, &a); err != nil {
		return payments.GrantAnswer{}, true, err
	}
	if secret != nil {
		plain, err := m.unseal(operationID, secret)
		if err != nil {
			return payments.GrantAnswer{}, true, err
		}
		if err := json.Unmarshal(plain, &a.Env); err != nil {
			return payments.GrantAnswer{}, true, err
		}
	} else if a.Status == payments.GrantReady && a.KeyHint != "" {
		// The provider asked again after the deadline, when the key is gone: nothing it
		// accepts can be sent, and it refunds or keeps the first answer regardless.
		return payments.GrantAnswer{}, true, errors.New("marketplace: a provision repeated after its credentials were wiped")
	}
	return a, true, nil
}

// record stores an answer for its operation, with a provision's credentials sealed
// until the request's deadline.
func (m *Marketplace) record(ctx context.Context, tx *sql.Tx, req payments.GrantRequest, r result) error {
	stored, err := json.Marshal(r.answer)
	if err != nil {
		return err
	}
	var secret []byte
	var until *time.Time
	if len(r.env) > 0 {
		plain, err := json.Marshal(r.env)
		if err != nil {
			return err
		}
		if secret, err = m.seal(req.OperationID, plain); err != nil {
			return err
		}
		deadline := req.Deadline
		if deadline.IsZero() {
			deadline = m.now().Add(24 * time.Hour)
		}
		deadline = deadline.Add(secretGrace)
		until = &deadline
	}
	var grantRef, orderID sql.NullString
	if r.grantID != "" {
		grantRef = sql.NullString{String: r.grantID, Valid: true}
	}
	if req.OrderID != "" {
		orderID = sql.NullString{String: req.OrderID, Valid: true}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO marketplace_operations (
		    provider, operation_id, grant_ref, event_id, action, order_id, status, answer,
		    secret, secret_until, test
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		m.provider.Name(), req.OperationID, grantRef, req.EventID, string(req.Action), orderID,
		string(r.answer.Status), stored, secret, until, req.Test,
	)
	return err
}

// failed refuses a request: the provider refunds the buyer. review, if set, asks an
// operator to look.
func failed(reason, review string) result {
	return result{answer: payments.GrantAnswer{Status: payments.GrantFailed, FailureReason: reason}, review: review}
}

func ready() result {
	return result{answer: payments.GrantAnswer{Status: payments.GrantReady}}
}

// checkOrder finds the product an order bought, and refuses an order the app must not
// grant.
func (m *Marketplace) checkOrder(req payments.GrantRequest) (Product, *result) {
	p, ok := m.products[req.ProductID]
	if !ok {
		r := failed("This product is not sold here.", "marketplace_unknown_product")
		return Product{}, &r
	}
	if !req.OrderPaid || req.Currency != "EUR" || req.AmountCents < p.PriceCents {
		r := failed("The order does not match the product's price.", "marketplace_order_mismatch")
		return Product{}, &r
	}
	return p, nil
}

// lockGrant reads and locks the request's grant, if there is one.
func (m *Marketplace) lockGrant(ctx context.Context, tx *sql.Tx, grantID string) (grant, bool, error) {
	var g grant
	err := tx.QueryRowContext(ctx, `
		SELECT id, account_id, key_id, order_id, test,
		       claimed_at IS NOT NULL, revoked_at IS NOT NULL, ended_at IS NOT NULL
		FROM marketplace_grants
		WHERE provider = $1 AND grant_id = $2
		FOR UPDATE`, m.provider.Name(), grantID,
	).Scan(&g.id, &g.accountID, &g.keyID, &g.orderID, &g.test, &g.claimed, &g.revoked, &g.ended)
	if errors.Is(err, sql.ErrNoRows) {
		return grant{}, false, nil
	}
	return g, err == nil, err
}

// provision opens an account without an identity, issues its key and claim link, and
// credits the order.
func (m *Marketplace) provision(ctx context.Context, tx *sql.Tx, req payments.GrantRequest) (result, error) {
	var p Product
	if !req.Test {
		var refused *result
		if p, refused = m.checkOrder(req); refused != nil {
			return *refused, nil
		}
	}
	g, found, err := m.lockGrant(ctx, tx, req.GrantID)
	if err != nil {
		return result{}, err
	}
	if found && g.active() {
		return failed("This buyer already has an account; buy a top-up instead.", "marketplace_repeated_provision"), nil
	}
	account, err := m.accounts.CreateWithoutIdentity(ctx, tx)
	if err != nil {
		return result{}, err
	}
	raw, key, err := m.keys.CreateTx(ctx, tx, account.ID, m.cfg.KeyName)
	if err != nil {
		return result{}, err
	}
	token, hash, err := claimToken()
	if err != nil {
		return result{}, err
	}
	var orderID sql.NullString
	if req.OrderID != "" {
		orderID = sql.NullString{String: req.OrderID, Valid: true}
	}
	if found {
		// A grant whose account ended starts again with a new one.
		_, err = tx.ExecContext(ctx, `
			UPDATE marketplace_grants
			SET account_id = $2, key_id = $3, order_id = $4, claim_token_hash = $5, test = $6,
			    created_at = now(), claimed_at = NULL, revoked_at = NULL, ended_at = NULL
			WHERE id = $1`, g.id, account.ID, key.ID, orderID, hash, req.Test,
		)
	} else {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO marketplace_grants (provider, grant_id, account_id, key_id, order_id, claim_token_hash, test)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id`, m.provider.Name(), req.GrantID, account.ID, key.ID, orderID, hash, req.Test,
		).Scan(&g.id)
	}
	if err != nil {
		return result{}, err
	}
	if !req.Test {
		t, err := m.credits.Lock(ctx, tx, account.ID)
		if err != nil {
			return result{}, err
		}
		if _, _, err := t.CreditOrder(m.order(req, p)); err != nil {
			if errors.Is(err, credits.ErrConflict) {
				return failed("This order was already delivered.", "marketplace_order_conflict"), nil
			}
			return result{}, err
		}
	}
	r := ready()
	r.grantID = g.id
	r.env = map[string]string{m.cfg.KeyEnv: raw, m.cfg.ClaimEnv: m.claimLink(token)}
	r.answer.KeyHint = key.Suffix
	r.answer.Entitlement = m.cfg.Entitlement(Entitlement{CreditCents: p.CreditCents, InactiveMonths: m.cfg.InactiveMonths,
		ClaimEnv: m.cfg.ClaimEnv})
	return r, nil
}

// order describes a request's order as a credit purchase.
func (m *Marketplace) order(req payments.GrantRequest, p Product) credits.Order {
	return credits.Order{
		Provider:    m.provider.Name(),
		OrderID:     req.OrderID,
		EventID:     req.EventID,
		CreditCents: p.CreditCents,
		PriceCents:  p.PriceCents,
		PaidCents:   req.AmountCents,
		PaidAt:      req.PaidAt,
		Description: m.cfg.Description,
	}
}

// topUp credits another order to the grant's account.
func (m *Marketplace) topUp(ctx context.Context, tx *sql.Tx, req payments.GrantRequest) (result, error) {
	g, found, err := m.lockGrant(ctx, tx, req.GrantID)
	if err != nil {
		return result{}, err
	}
	if !found || !g.active() {
		return failed("This buyer has no account to top up.", "marketplace_top_up_without_account"), nil
	}
	r := ready()
	r.grantID = g.id
	if req.Test || g.test {
		return r, nil
	}
	p, refused := m.checkOrder(req)
	if refused != nil {
		refused.grantID = g.id
		return *refused, nil
	}
	t, err := m.credits.Lock(ctx, tx, g.accountID.String)
	if err != nil {
		return result{}, err
	}
	if _, _, err := t.CreditOrder(m.order(req, p)); err != nil {
		if errors.Is(err, credits.ErrConflict) {
			r := failed("This order was already delivered.", "marketplace_order_conflict")
			r.grantID = g.id
			return r, nil
		}
		return result{}, err
	}
	return r, nil
}

// revoke ends a grant whose provision was refunded or charged back: it takes back the
// provision's credit, revokes the key and, while nobody has claimed the account,
// deletes it.
func (m *Marketplace) revoke(ctx context.Context, tx *sql.Tx, req payments.GrantRequest) (result, error) {
	g, found, err := m.lockGrant(ctx, tx, req.GrantID)
	if err != nil || !found {
		return ready(), err
	}
	r := ready()
	r.grantID = g.id
	if !g.accountID.Valid || g.ended {
		return r, nil
	}
	t, err := m.credits.Lock(ctx, tx, g.accountID.String)
	if err != nil {
		return result{}, err
	}
	if g.orderID.Valid && !g.test {
		_, err := t.ReverseOrder(m.provider.Name(), g.orderID.String, req.EventID, req.Reason == "chargeback")
		if err != nil && !errors.Is(err, credits.ErrNotFound) {
			return result{}, err
		}
	}
	if g.keyID.Valid {
		if err := m.keys.RevokeTx(ctx, tx, g.accountID.String, g.keyID.String); err != nil && !errors.Is(err, apikeys.ErrNotFound) {
			return result{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE marketplace_grants
		SET revoked_at = COALESCE(revoked_at, now()), claim_token_hash = NULL
		WHERE id = $1`, g.id,
	); err != nil {
		return result{}, err
	}
	if !g.claimed {
		if err := m.end(ctx, tx, g); err != nil {
			return result{}, err
		}
	}
	return r, nil
}

// revokeCredit takes back what a refunded or charged-back top-up added.
func (m *Marketplace) revokeCredit(ctx context.Context, tx *sql.Tx, req payments.GrantRequest) (result, error) {
	g, found, err := m.lockGrant(ctx, tx, req.GrantID)
	if err != nil || !found {
		return ready(), err
	}
	r := ready()
	r.grantID = g.id
	if !g.accountID.Valid || g.test || req.Test || req.OrderID == "" {
		return r, nil
	}
	t, err := m.credits.Lock(ctx, tx, g.accountID.String)
	if err != nil {
		return result{}, err
	}
	if _, err := t.ReverseOrder(m.provider.Name(), req.OrderID, req.EventID, req.Reason == "chargeback"); err != nil &&
		!errors.Is(err, credits.ErrNotFound) {
		return result{}, err
	}
	return r, nil
}

// end deletes a grant's unclaimed account inside tx, with every OnDelete hook, and
// records that the grant ended.
func (m *Marketplace) end(ctx context.Context, tx *sql.Tx, g grant) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE marketplace_grants
		SET ended_at = now(), claim_token_hash = NULL
		WHERE id = $1`, g.id,
	); err != nil {
		return err
	}
	err := m.accounts.DeleteWithoutIdentity(ctx, tx, g.accountID.String)
	if errors.Is(err, accounts.ErrNotFound) {
		return nil
	}
	return err
}

// claimToken returns a new claim token and the hash stored for it.
func claimToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// claimLink is the claim page's URL with a token.
func (m *Marketplace) claimLink(token string) string {
	return m.cfg.ClaimURL + "?token=" + token
}

// isInvalidUUID reports PostgreSQL's error for a malformed UUID parameter.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}
