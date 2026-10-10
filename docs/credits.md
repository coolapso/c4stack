# Credits contract

The `credits` module keeps prepaid credit for accounts. This page lists what it
guarantees, what the app decides, and what each payment webhook does.

## Money

- One credit is one EUR cent. Amounts are integers.
- The server prices every purchase. The browser sends the amount of credit it wants,
  and the server checks it against `Policy` and computes the fee. The payment provider
  never decides what is credited.
- `Policy` holds the app's business values: the purchase limits and the fee. The
  framework has no defaults for them.

## Ledger rules

Every change to credit is a row in `credit_ledger`:

| Kind | Sign | Written by |
|---|---|---|
| `purchase` | + | the webhook, for a paid purchase |
| `refund`, `dispute` | − | the webhook, for a reversed purchase |
| `debit` | − | the app, for work (`Debit`, `Tx.Debit`) |
| `adjustment` | ± | the app or an operator (`Adjust`, `credits adjust`) |
| `expiry` | − | credit expiry, when that module is enabled |
| `forfeit` | − | account deletion, for remaining credit |

These rules hold:
- **Every mutation locks the account row.** Concurrent debits cannot overdraw.
- **Every mutation is idempotent per account and reference.** Repeating a debit or
  adjustment with the same reference and amount succeeds without a second entry. The
  same reference with another amount returns `ErrConflict`. References starting with
  `purchase:`, `refund:`, `dispute:`, `expiry:` or `forfeit:` are reserved.
- **Debits never take available credit below zero.** Available credit is the balance
  minus what the app's `Holds` function reports. Adjustments and reversals can make the
  balance negative.
- **The ledger is append-only.** A trigger rejects changes and deletes. Corrections are
  new entries.
- **The one exception is account deletion.** The remaining positive balance is
  forfeited first, then the account's entries and purchases lose their link to the
  account and the entries lose their descriptions. The amounts stay, for accounting.

## Holds

An app that reserves credit for work in progress keeps the reservations in its own
table and reports them through `Config.Holds`. To reserve or capture:

```go
err := cr.WithAccount(ctx, accountID, func(tx *credits.Tx) error {
	avail, err := tx.Available()
	if err != nil {
		return err
	}
	if cost > avail {
		return credits.ErrInsufficient
	}
	_, err = tx.SQL().ExecContext(ctx, `INSERT INTO jobs (id, account_id, reserved_cents) VALUES ($1, $2, $3)`, jobID, accountID, cost)
	return err
})
```

`WithAccount` locks the account first, so lock nothing of the app's before it. The app's
changes and the framework's debits commit together or not at all. Do no remote calls
inside it.

## Payment webhooks

The webhook credits what the stored purchase says, never what the event says. It
finds the purchase by the reference sent to the checkout, then by the provider's
checkout ID, then by its order ID.

| Event | Situation | Response | Effect |
|---|---|---|---|
| Any | Signature or body invalid | 400 | Nothing |
| Any | Not about this app's payments | 200 | Nothing |
| Any | Database or other transient failure | 503 | Nothing; the provider retries |
| Paid | Pending purchase, amount and currency match | 200 | Purchase paid; credit granted once |
| Paid | Already paid, refunded or disputed | 200 | Nothing |
| Paid | Amount, currency or checkout differs, or a discount lowered the payment | 200 | Review |
| Paid | The account was deleted | 200 | Purchase paid, no credit; review for a refund |
| Refund or dispute | Paid purchase, full amount | 200 | Credit reversed once, even below zero |
| Refund or dispute | Already reversed | 200 | Nothing |
| Refund or dispute | Partial amount, or currency differs | 200 | Review |
| Refund or dispute | Purchase not yet paid | 200 | Review |
| Refund or dispute | The account was deleted | 200 | Purchase marked reversed; no ledger change |
| Any | No matching purchase | 200 | Review |
| Any | The adapter flagged it, such as a refund that has not succeeded | 200 | Review |

Creem retries a failed delivery after 30 seconds, 5 minutes, 30 minutes and 6 hours, then
gives up. A failure that lasts longer needs the operator: compare Creem's dashboard with
`credits reviews` and adjust.

Logs carry event, purchase and account IDs and the outcome. They never contain webhook
bodies, buyer names or email addresses.

## Reviews

A review is an event that was not applied, with a fixed reason. Operators handle them
with the app's commands:

```sh
acme credits reviews                          # list open reviews
acme credits adjust <account-id> <cents> <reference> <description…>
acme credits resolve <review-id> <what you did>
```

Common cases:
- **`partial_reversal`**: decide how much credit to remove, and `credits adjust` it.
- **`reversal_before_payment`**: wait for the paid event; once the credit was granted,
  remove it with `credits adjust`.
- **`paid_after_account_deleted`**: refund the payment in the provider's dashboard.
- **A dispute won** (the bank returned the money): restore the credit with
  `credits adjust` and a unique reference.
