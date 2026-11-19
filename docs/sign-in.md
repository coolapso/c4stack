# Sign-in

**Status: built, in `identity/local` and `email/smtp`.** `c4stack new` generates apps on
it, and the example app, Textlab, uses it.

This is the design of C4Stack's own sign-in. It is a passwordless identity provider:
people sign in with a code sent to their email address, or with a passkey. Each app
keeps its users, sessions and passkeys in its own database. Development therefore works
offline, staging is just another database, and integration tests run the real flows.

It is a new package beside `identity/hanko`, which stays. Like any provider, it plugs
into `accounts`, so accounts, deletion and every module work as they do now.

Most of the risk is in the flows, not the cryptography: someone taking over an account
by guessing, replaying or relaying a code, by stealing a session, by racing a change, or
by changing an account's address or passkeys. This document is written so that each of
those paths can be checked: [Threats](#threats) lists them and what stops each one.

Never in this module: passwords, social login, SAML, MFA screens.

## At a glance

| | |
|---|---|
| Package | `identity/local`, provider name `local` |
| Signs in with | an 8-digit code sent by email, or a passkey (WebAuthn, `go-webauthn/webauthn`) |
| Signs up with | the same code flow: confirming a code for a new address creates the user and its account |
| Sessions | in the app's database, stored as hashes, revocable, in a `__Host-session` cookie |
| Other cookies | sealed: `__Host-signin` (a code attempt), `__Host-passkey` (a passkey ceremony), `__Host-device` (a browser that signed in before) |
| Tables | `local_users`, `local_sessions`, `local_passkeys`, `local_codes`, `local_limits`, `local_used_challenges`, `local_notices` |
| Pages | `/login` and its steps; a profile UI on the settings page: address, passkeys, signed-in devices |
| Commands | `local sign-out <user>`, `local unblock <address>`, `local set-email <user> <new address>`, where `<user>` is a user ID or an address |
| Background jobs | security notices every minute; cleanup of expired rows every hour |
| Needs | an `email.Sender` (Scaleway's API in production, SMTP to Mailpit in development), and the app's secret key |

## How it fits

The package implements `identity.Provider` (`Name`, `Authenticate`, `Logout`,
`Delete`), and the profile UI that `accounts` looks for (`MountProfile`, `ProfileCSP`,
`ProfileSection`), as the Hanko adapter does. A sign-in's subject is the local user's ID,
and its address is always verified, because every address got there by confirming a
code. A new local identity joins the account that has its verified address. That is how
an app moves from Hanko to this provider without anyone losing their account.

**Dependencies.** `identity/local` depends on `identity`, `app`, `web`, `ui` and `email`.
It is the first provider with tables, so it needs `app`, as modules do: for its
migrations, its commands and its jobs. Nothing depends on it. `accounts` still depends on
`identity` alone.

### One transaction with `accounts`

With Hanko, the provider and `accounts` are two systems. `accounts` copies the address
Hanko reports on each request, and deletes in two steps. This provider's users live in
the same database as the accounts, so every change that touches both can be one
transaction. A new optional interface in `identity` makes that possible, without
`identity/local` depending on `accounts`:

```go
// TxProvider is a provider that keeps its users in the app's database. accounts lends it
// hooks that run inside the provider's own transactions, and deletes its users inside
// the transaction that deletes the account.
type TxProvider interface {
	Provider
	// UseAccounts receives the hooks. accounts.New calls it.
	UseAccounts(h AccountHooks)
	// SessionTx locks the user of the request's session inside tx, and returns the
	// session as it is then: ErrNoSession if it ended meanwhile.
	SessionTx(ctx context.Context, tx *sql.Tx, r *http.Request) (Identity, error)
	// DeleteTx deletes the user with this subject inside tx.
	DeleteTx(ctx context.Context, tx *sql.Tx, subject string) error
}

// AccountHooks are accounts' side of a sign-in and of an address change.
type AccountHooks struct {
	// SignedIn finds, joins or creates the identity's account inside tx. An error turns
	// the sign-in away, and the provider rolls it back.
	SignedIn func(ctx context.Context, tx *sql.Tx, id Identity) error
	// AddressChanged records the identity's new verified address and makes it the
	// account's primary address, inside tx. It fails if another account has it.
	AddressChanged func(ctx context.Context, tx *sql.Tx, subject, email string) error
}
```

When the provider is a `TxProvider`:
- **Signing up** creates the local user, its account and its session in one
  transaction. No local user is ever left without an account.
- **An address change**, the person's own or support's, changes the local user, the
  identity's recorded address and the account's primary address in one transaction.
- **`RequireSignIn` never writes addresses from what `Authenticate` returned.** Addresses
  change only through `AddressChanged`. Otherwise a request that authenticated just
  before an address change could write the old address back after it. Someone holding
  the old mailbox could then sign up again with it and join the account.
- **Deletion** is one transaction. `accounts.Delete` locks the user through the
  request's session with `SessionTx`, and rechecks that the session still exists and is
  recent, so a deletion that waited behind a recovery deletes nothing. It deletes the
  provider's users, then locks the account, records the tombstones, runs the `OnDelete`
  hooks and deletes the account. Sessions, passkeys and codes go with the user. It all commits or nothing
  does, so the deletion marks aren't needed. Afterwards `Logout` clears the cookie.
  `deleteTx` makes the same call, so `accounts delete-local` and the other paths that
  finish a deletion remove the local user too.

Without `accounts`, no hooks are set, and a sign-in creates only the user and its
session.

### One change to a user at a time

Every transaction that issues a session, adds or removes a passkey, changes an address,
ends sessions or deletes a user takes its locks in one order:
1. the local user's row: `FOR NO KEY UPDATE`, which serialises every change to the user
   but lets other requests insert the user's codes and sessions, or deleting it, which
   takes `FOR UPDATE`;
2. the code or passkey row it uses;
3. the limits' counters it touches;
4. what `accounts` locks through its hooks.

Inside the transaction, it rechecks what authorised it:
- the code still exists, is unexpired, and was sent to this user at this address;
- the passkey still exists and belongs to the user;
- the session that asked for a settings change still exists, and is still recent where
  that is needed.

So a sign-in or a passkey registration that started before a recovery or a deletion, and
commits after it, finds what authorised it gone, and is refused. One that commits first
is undone by the recovery, which ends every session and removes every passkey. If an
implementation ever takes locks out of this order, PostgreSQL ends one of the deadlocked
transactions: the request fails and can be retried, and nothing is half done.

## The flows

Pages render in the app's layout and work without JavaScript, except passkeys, which
need the browser's WebAuthn API. The module's own small script drives them, served from
the app's assets under `script-src 'self'`. Every form uses the CSRF token and the
Origin check. Signed-in routes get both from `RequireSignIn`, and the sign-in pages use
`web.CSRF`.

Addresses are trimmed and lowercased before anything else. They must parse as one plain
address (no display name) of at most 254 bytes. The lowercase address is stored and
compared everywhere.

### Signing up and signing in with a code

One flow does both. It answers the same way whether or not the address has an account.

1. **`GET /login`** asks for an email address (`autocomplete="username webauthn"`) and
   offers "Sign in with a passkey". A visitor who already has a valid session continues
   to `next`, which is a local path or `SignedInPath`.
2. **`POST /login`** with the address:
   1. checks and counts the limits in one transaction ([The limits](#the-limits)): this
      browser's own counters if its device cookie names the address's user, the
      address's shared counters if not, and the network's and the app's;
   2. makes a code: 8 random digits, valid for 10 minutes, stored as a keyed hash. If the
      address has a user, the code records that user, and which counters it was sent
      under: this browser's own or the address's;
   3. emails it: the sign-in code if the address has a user, the sign-up code if not;
   4. sets the attempt cookie (`__Host-signin`, sealed: the attempt's ID, the code's ID,
      the address and `next`). It lives 30 minutes, longer than the code, so the page can still say that
      the code expired and offer a new one;
   5. redirects to `/login/code`.

   Only the mailbox's owner sees which email went out. The page is the same either way.
3. **`GET /login/code`** says "We sent a code to ann@example.com. It works for 10
   minutes." It has a field for the code (`inputmode="numeric"`,
   `autocomplete="one-time-code"`), "Send a new code" and "Use another address". Without
   an attempt cookie, it goes back to `/login`.
4. **`POST /login/code`** takes the code with or without the space, and runs one
   transaction:
   1. It reads the attempt's code. If the code records a user, it locks that user's row,
      then the code row. Otherwise it locks the code row. Two submissions of one code
      therefore cannot both succeed.
   2. If the code is missing or expired: "This code has expired. Send a new one." A code
      that records a user works only while that user exists and still has the address
      the code went to. If not: "This code is no longer valid. Send a new one."
   3. If the code is wrong, it adds a wrong entry to the code, and to the counters the
      code was sent under. "That code isn't right. 3 tries left." On the fifth wrong
      entry the code is deleted: "Too many wrong codes. Send a new one."
   4. If the code is right, it deletes the code, then finds the address's user or
      creates one. A concurrent sign-up of the same address finds the other's user,
      because the address is unique. It runs `SignedIn`, which finds, joins or creates
      the account. If `accounts` turns the identity away, such as for an account being
      deleted, everything rolls back, the page says that this account can't be signed in
      to right now, and the log says why. Finally it ends the session
      this browser had, if any, and creates a session.
   5. It commits, sets `__Host-session` and `__Host-device`, clears the attempt cookie
      and redirects to `next`.
5. **`POST /login/code/resend`** replaces the attempt's code with a new one: no sooner
   than 30 seconds after the last, and within the limits. Only the newest code of an
   attempt works: codes carry their attempt's ID, and a new one deletes the attempt's
   others under a lock on the attempt, so a replayed copy of an older cookie can't keep
   two working. Other attempts for the same address, from other browsers, keep their
   own codes, so someone who asks for codes for your address doesn't cancel yours.

When a limit stops a request, the page says when to try again. It also says that a
browser you've signed in on before, or a passkey, still works. The answer depends only
on the counters, never on whether the address has an account. If the email provider
refuses the message: "We couldn't send a code right now. Try again in a moment." The
code is then deleted, and the attempt still counts against the limits.

### Signing in with a passkey

Passkeys are discoverable credentials, and require user verification (the device's PIN,
fingerprint or face). They are only ever added by someone signed in.

1. The sign-in page's script asks `POST /login/passkey/options` for a ceremony. The
   sealed `__Host-passkey` cookie holds go-webauthn's whole session data (the challenge,
   the user verification it requires, its expiry) and the ceremony's purpose. It lives
   5 minutes. The script starts the browser's conditional UI, so passkeys are offered in
   the email field's autofill. "Sign in with a passkey" starts the same thing as a modal
   prompt.
   - The script starts a new ceremony before the challenge expires.
   - If another tab started a ceremony and replaced the cookie, this tab's attempt fails
     its check, and the script quietly starts again.
2. `POST /login/passkey` takes the assertion. go-webauthn checks it against the session
   data, the relying party ID, the app's origins and user verification.
3. Then one transaction:
   1. It locks the row of the user that the user handle names, then the passkey's row,
      and checks the passkey still exists and belongs to that user.
   2. **The counter.** A passkey that keeps a signature counter must report a higher
      number each time. If the stored and the reported numbers are both zero, the
      passkey keeps no counter, and that is accepted. If the reported number is higher,
      it is accepted and stored. Otherwise the passkey may have been copied: sign-in with
      it is refused and logged with the user and passkey IDs, and the person can still
      sign in with a code. The module applies this rule itself, from go-webauthn's
      `CloneWarning` and the two numbers, rather than taking a validated assertion as
      proof that it passed.
   3. It records the challenge's hash. A challenge recorded already is refused, so a
      captured assertion cannot be replayed with a copy of the cookie.
   4. As with a code, it runs `SignedIn`, ends this browser's old session and creates a
      session.
4. It commits, sets the session and device cookies, and the script goes to `next`.

Synced passkeys keep no counter, so the counter rule catches copies only of passkeys
that do.

### Confirming it's you

Some actions need a recent sign-in: within `identity.RecentSignIn`, 5 minutes. Those
are adding or removing a passkey, changing the address and deleting the account.
Sessions are long, so a browser left signed in, or a stolen session, must not be enough.

`GET /login/confirm?next=…` is a sign-in for the signed-in address only, with a passkey
or with a code to that address. It is the same flow with the address fixed. Its passkey
ceremony's purpose is `confirm`, and it names the user and the session that asked. It
replaces the session with a new one, so the new sign-in time is real. The profile UI
links there when an action needs it. The `accounts` delete page keeps its current way
(sign out, then sign in), and can link here instead (see
[What changes elsewhere](#what-changes-elsewhere)).

### Sessions and signing out

- A session is 32 random bytes in `__Host-session` (`HttpOnly`, `Secure`,
  `SameSite=Lax`, `Path=/`). The cookie expires at the end of the session's 90 days. The
  database keeps only the token's SHA-256.
- `Authenticate` looks the hash up on every request. A session ends 30 days after it was
  last used, and 90 days after its sign-in whatever happens. Its last use is written at
  most once an hour, so the idle end can come up to an hour late. `SignedInAt` is the
  session's creation time.
- Every sign-in creates a new session and ends the one the browser had, so a planted
  cookie never becomes a signed-in session.
- `Logout` deletes the session and clears the cookie. It clears the cookie even when the
  database fails.
- Each session records a device label, such as "Firefox on Linux", made from the user
  agent at sign-in, and how it signed in, `code` or `passkey`. No IP address is stored.
- `Authenticate` reports how the session signed in as `Identity.Method`. An app can then
  ask for a passkey where it matters, such as an admin area, and an OIDC provider can
  report it (`amr`).

### Browsers that signed in before

After every sign-in, the browser gets `__Host-device`: a sealed cookie with the user's ID
and when it was issued, for a year. It signs nobody in. It only chooses which counters
apply when that browser asks for a code:
- **A known browser** asking for a code for its user's address uses counters of its own,
  with the same values as the shared ones.
- **Everyone else** shares the address's counters.

Someone who asks for codes, or enters wrong ones, from elsewhere can only use up the
shared counters. The owner keeps signing in with codes on the browsers they used before.

A cookie issued before the user's `devices_since` doesn't count. Signing out everywhere
moves `devices_since` to now and gives the current browser a new cookie. That happens
when the person signs out all their other devices or changes their address, and with
support's `sign-out` and `set-email`. A stolen device cookie gives its holder at most the
same 20 wrong entries a window as the shared counters, and still no code.

### Passkeys in settings

- **Adding** needs a recent sign-in when it starts and when it finishes, so a prompt left
  open past the window asks to confirm again. The person names the passkey (up to 60
  characters). Registration excludes their existing passkeys, asks for a discoverable
  credential and user verification, and asks for no attestation. The user handle is the
  user's ID, never the address. The ceremony's purpose is `add`, and it names the user
  and the session that started it; the finish must come from that session. One
  transaction locks the user, rechecks the session and the 20-passkey limit, records the
  challenge, stores the passkey and queues a notice to the address.
- **Removing** needs a recent sign-in. One transaction removes it and queues a notice.
  Removing the last passkey is allowed, because codes still work.
- The list shows each passkey's name, when it was added and last used, and whether it is
  synced (its backup flag).

### Signed-in devices

The settings page lists the user's sessions: device, when it signed in, when it was
last active, and which one is this device. The person can sign out any of them, or all
but this one. Signing out all but this one also resets known browsers (see above). None
of this needs a recent sign-in, because it only takes access away.

### Changing the address

1. The person enters a new address, on a page of its own. This needs a recent sign-in.
2. The module makes an attempt either way, within the limits: the app's, the network's,
   the new address's own, and the user's new-address counters:
   - If no local user has the address, the code goes to it.
   - If another local user has it, the attempt is a decoy. Its code is never sent and
     never matches, and the address gets the "address taken" email instead.

   The page, wrong entries and their messages, resending and the limits are the same for
   both. The form therefore cannot tell anyone which addresses have accounts.
3. The person enters the code: 10 minutes, 5 wrong entries, one attempt per user at a
   time. Wrong entries count against the code and the network.
4. On the right code, one transaction:
   1. locks the user's row, and rechecks the code, and that the session asking still
      exists and is still recent;
   2. runs `AddressChanged`: `accounts` records the identity's new address and makes it
      the account's primary address. If another account has it: "This address belongs to
      another account", and nothing changes;
   3. changes the local user's address, and deletes the user's codes still waiting at the
      old address;
   4. ends every other session, and replaces this browser's session token with a new
      one, so any copy of the old token stops working too;
   5. moves `devices_since` to now, and gives this browser a new device cookie;
   6. queues the notice to the old address.

Passkeys stay. A password manager may still show the old address on them. The passkey
works anyway: it is found by credential, not by address.

### Deleting the account

Deletion goes through `accounts`, as now: a recent sign-in, typing `DELETE`, then the
single transaction described in [How it fits](#one-transaction-with-accounts).

### Operator commands

`<user>` is a user ID or the user's current address.

- `acme local sign-out <user>` ends every session of the user, and resets known browsers.
- `acme local unblock <address>` resets the address's shared counters, so codes work
  again at once.
- `acme local set-email <user> <new address>` is support's way back into an account. It
  is for someone who lost their mailbox and has no passkey, or whose address an attacker
  changed. One transaction:
  1. locks the user;
  2. runs `AddressChanged`, then sets the new address, which must not belong to another
     user;
  3. deletes every session, passkey and waiting code of the user, so nothing an
     attacker added survives;
  4. resets known browsers;
  5. queues a notice to each address.

  The person then signs in with a code to the new address and adds their passkeys again.

Each command prints what it did. `set-email` says whether its notices were accepted by
the email provider or are still queued.

**Support checks who is asking.** `set-email` trusts the operator, so the runbook in
`docs/operations.md` says what is never enough on its own:
- knowing the account's details;
- controlling the new address;
- a request sent from the new address.

The app's support policy says what is enough, such as payment records or earlier mail
from the old address. Every address-change notice carries a reference, the user's ID.
Someone whose address was changed behind their back can quote it, even though the
account no longer shows their address anywhere.

## The limits

The values are fixed in the module, like `identity.RecentSignIn`. They aren't business
values, and an app should not be able to weaken them. The one setting is an optional
ceiling on the codes the whole app sends in an hour (`Config.MaxCodesPerHour`). It can
only add a limit. Counters are kept in PostgreSQL, so every replica shares them. An
ingress cannot apply them, because it doesn't see addresses.

| Limit | Value | Prevents |
|---|---|---|
| Code length | 8 digits | Guessing. An address takes at most 20 wrong entries a window, and two windows can meet, so at most 40 in any 24 hours: a 1 in 2,500,000 chance. |
| Code lifetime | 10 minutes | Using an old code found later in a mailbox |
| Wrong entries per code | 5, then the code is gone | Guessing one code |
| Wrong entries per address | 20 a window of 24 hours, then the shared counters stop codes for the address (none sent or accepted) until the window ends | Guessing across many codes |
| Wrong entries per network | 100 an hour | Guessing across many addresses from one machine |
| A new code for the same attempt | 30 seconds after the last | Resend loops |
| Codes per address | 5 an hour, 10 a day | Flooding someone's mailbox through the app |
| Codes per network | 30 an hour | One machine making the app email many addresses, which hurts the sending domain |
| Codes for the whole app | `Config.MaxCodesPerHour`, optional | Many networks together using up the sending domain's reputation or the provider's quota |
| New-address codes per user | 5 an hour, 10 a day | A signed-in account flooding a stranger's mailbox |
| A known browser | its own per-address counters, with the same values | Someone else using up the owner's counters |
| Device cookie | a year, until the user signs out everywhere | Old browsers keeping their own counters for ever |
| Session idle lifetime | 30 days | A forgotten browser staying signed in |
| Session lifetime | 90 days | A stolen session lasting for ever |
| Recent sign-in | 5 minutes (`identity.RecentSignIn`) | A stolen or unattended session changing passkeys, the address, or deleting the account |
| Passkey challenge | 5 minutes, single use | Replaying a captured assertion |
| Passkeys per user | 20 | Unbounded rows |
| Passkeys a user adds | 10 a day; removing isn't limited | Adding and removing again and again, each with a notice, flooding the sending domain. Removals stay bounded by what can be added, and someone who added passkeys can't stop the owner removing them. |
| Sessions per user | 50; a new sign-in signs out the least recently used | Rows piling up from sign-ins that drop their cookies, such as a script signing in with a passkey again and again |

**How counters are kept.**
- Counters use fixed windows: a window starts at its first event. Each counter's key is
  an HMAC of the limit's name and what it counts (an address, a network, a known
  browser, a user, the app), so the table holds no addresses.
- One transaction checks and counts every counter a request touches, in a fixed order:
  the app, the network, the address or the known browser, the user. Each counter is one
  upsert, which starts a new window when the old one has ended and returns the count.
- If any counter is over its limit, the transaction rolls back, so a refused request
  counts nowhere.
- "Address taken" emails and decoy attempts count exactly as codes do.
- The upsert resets ended windows itself, so the hourly cleanup only removes old rows. It
  never decides whether a limit applies.

**Networks** are an IPv4 address or an IPv6 /64, taken from the client IP as
`web.Config.TrustedProxies` makes it. Getting that setting wrong breaks the network
limits either way, and `docs/operations.md` must say so:
- trusting too few proxies makes every request seem to come from the proxy, so the
  limits hit everyone at once;
- trusting forwarding headers from anyone lets an attacker choose the network they seem
  to come from.

Passkey sign-in has no limit of its own: an assertion cannot be guessed, and starting a
ceremony costs the server nothing but a cookie.

**What a determined attacker can still do.** By asking for codes and entering wrong ones,
someone can use up an address's shared counters every day, for as long as they keep at
it. Codes for that address then work only on browsers that signed in before. Passkeys
work as always. A code-only user on a new browser stays locked out while the attack
goes on. `local unblock` resets the shared counters, which helps once, but not against
someone who keeps going. When the wrong-entry limit trips and the address has a user,
that user gets one notice per window.

## The tables

The migration namespace is `local`, with one initial migration before v0.1.0, and every
statement is idempotent.

```sql
-- One user per address that confirmed a code.
CREATE TABLE IF NOT EXISTS local_users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email text NOT NULL UNIQUE, -- lower case
    -- Device cookies issued before this don't count.
    devices_since timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS local_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES local_users (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE, -- SHA-256 of the cookie's token
    method text NOT NULL,             -- 'code' or 'passkey'
    device text NOT NULL,             -- "Firefox on Linux"
    created_at timestamptz NOT NULL DEFAULT now(), -- the sign-in
    last_seen_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_local_sessions_user_id
ON local_sessions (user_id);

CREATE TABLE IF NOT EXISTS local_passkeys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES local_users (id) ON DELETE CASCADE,
    credential_id bytea NOT NULL UNIQUE,
    public_key bytea NOT NULL, -- COSE
    sign_count bigint NOT NULL,
    aaguid uuid,
    transports text[] NOT NULL DEFAULT '{}',
    backup_eligible boolean NOT NULL,
    backed_up boolean NOT NULL,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);

CREATE INDEX IF NOT EXISTS idx_local_passkeys_user_id
ON local_passkeys (user_id);

-- Codes waiting to be entered. A code is deleted when it is used, replaced or expired,
-- and with its user.
CREATE TABLE IF NOT EXISTS local_codes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    purpose text NOT NULL, -- 'sign_in' or 'new_email'
    email text NOT NULL,   -- where it was sent
    -- sign_in: the address's user when the code was sent, if it had one.
    -- new_email: the user whose address changes.
    user_id uuid REFERENCES local_users (id) ON DELETE CASCADE,
    code_hash bytea NOT NULL, -- HMAC of the code's ID and the code
    decoy boolean NOT NULL DEFAULT false, -- a change to a taken address: never matches
    -- Which counters wrong entries count against: the address's when empty, else the
    -- known browser's own.
    device text NOT NULL DEFAULT '',
    attempt text NOT NULL DEFAULT '', -- sign_in: the browser's attempt
    wrong_entries integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_local_codes_user_id
ON local_codes (user_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_local_codes_new_email
ON local_codes (user_id)
WHERE purpose = 'new_email';

-- The limits' counters. key is an HMAC of the limit's name and what it counts.
CREATE TABLE IF NOT EXISTS local_limits (
    key bytea PRIMARY KEY,
    count integer NOT NULL,
    window_ends_at timestamptz NOT NULL
);

-- Passkey challenges already answered, kept until they expire.
CREATE TABLE IF NOT EXISTS local_used_challenges (
    challenge_hash bytea PRIMARY KEY,
    expires_at timestamptz NOT NULL
);

-- Security notices, written in the transaction that made the change. A notice is
-- deleted once the email provider accepts it, or after 3 days.
CREATE TABLE IF NOT EXISTS local_notices (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL, -- for logs; no foreign key, so a deletion doesn't drop it
    kind text NOT NULL,
    recipient text NOT NULL,
    data jsonb NOT NULL, -- what the email says: addresses, passkey name, device, time
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);
```

**What is stored, and for how long.**
- **Addresses are stored in clear**:
  - a user's address, until the account is deleted;
  - a code's address, for at most 10 minutes plus the time until the hourly cleanup.
    That includes the address of someone who asked for a sign-up code and never used
    it;
  - a notice's recipient and the addresses it mentions, until it is sent, and for at
    most 3 days.
- **Sessions** keep a device label and times, until they end: sign-out, expiry or
  deletion.
- **Passkeys** keep public keys only. They stay until removed.
- **Counters and used challenges** hold no personal data in clear.
- **A device cookie** holds the user's ID, sealed, in the browser.

**Hashes.** Codes and counter keys are HMACs, keyed with a key derived from the app's
secret key (HKDF, its own label). A copy of the database therefore reveals no codes, and
can't be used to guess them offline. Codes have only 10^8 values, so a plain hash would
be no protection. Counter keys reveal no addresses or networks, but the addresses are in
the other tables anyway. Session tokens have 256 bits, so plain SHA-256 is enough, and
rotating the secret key leaves sessions alone.

**Rotating the secret key** fails the codes then waiting, and the person sends a new
one. It also resets the counters, and makes the sealed cookies unreadable: attempts and
ceremonies start again, and known browsers fall back to the shared counters until they
sign in again. During a rolling rotation, replicas hold different keys. A code made on
one may fail on another, and the counters are split in two, so the limits are briefly
doubled. Rotate on every replica together where that matters.

## The emails

Every email is plain text, to one recipient, sent through the app's `email.Sender` from
the app's own domain. The app's name is in each subject and C4Stack's in none. The words
below are the defaults. An app brings its own words, look and templates through
`Config.Message`, as `credits/expiry` does.

**Codes** are sent at once, in the request, because the person is waiting for them.

**Security notices** are queued in `local_notices`, in the transaction that made the
change, so the change and its notice commit together.
- The request sends the notice right after the commit, with a context detached from the
  request. Closing the tab therefore cannot stop it.
- A job every minute retries those that are still queued, with growing gaps.
- A notice still unsent after 3 days is dropped, and the drop is logged with its kind
  and the user's ID.
- Notices stay out of the deletion transaction: a deleted user's queued notices are
  still sent, and deleted like any other, once accepted or after 3 days at most.

Times are in UTC, written as "19 November 2026 at 14:02 UTC".

No code is ever in a subject line, where a locked phone would show it. Code emails say
which site the code is for, to make it harder to phish (see [Threats](#threats)).

**Sign-in code**, to an address with a user.

> **Subject:** Your Acme sign-in code
>
> Hello,
>
> Here is your code to sign in to Acme:
>
> &nbsp;&nbsp;&nbsp;&nbsp;4821 0937
>
> Enter it at acme.example, on the page where you asked for it. It works once, within
> 10 minutes.
>
> If you didn't ask for it, ignore this email: nobody can sign in without the code.
> Never give it to anyone. Acme will never ask you for it.

**Sign-up code**, to an address without a user.

> **Subject:** Your Acme sign-up code
>
> Hello,
>
> Here is your code to create an Acme account with this address:
>
> &nbsp;&nbsp;&nbsp;&nbsp;4821 0937
>
> Enter it at acme.example, on the page where you asked for it. It works once, within
> 10 minutes.
>
> If you didn't ask for it, ignore this email: no account is created without the code.

**New-address code**, to the address someone wants to change to.

> **Subject:** Confirm your new address for Acme
>
> Hello,
>
> Someone signed in to Acme asked to use this address for their account. To confirm it,
> enter this code at acme.example:
>
> &nbsp;&nbsp;&nbsp;&nbsp;4821 0937
>
> It works once, within 10 minutes. If you didn't ask for it, ignore this email: the
> address isn't used without the code.

**Address taken**, instead of a code, when another user already has the new address.

> **Subject:** Your address and Acme
>
> Hello,
>
> Someone signed in to Acme asked to use this address for their account. This address
> already has an Acme account, so nothing changes, and that account stays as it is.
>
> If it was you, sign in with this address to use that account, or choose another
> address.

**Address changed**, to the old address.

> **Subject:** Your Acme address was changed
>
> Hello,
>
> The address of your Acme account was changed from ann@example.com to
> ann@example.org on 19 November 2026 at 14:02 UTC, from Firefox on Linux. Sign-in codes
> now go to the new address, and your other devices were signed out.
>
> If you made this change, there is nothing to do.
>
> If you didn't, someone else may be signed in to your account. Reply to this email
> right away, and quote this reference: 6f1c2e5a-0b7d-4c1e-9a53-2d8e4f7b1c90

**Passkey added.**

> **Subject:** A passkey was added to your Acme account
>
> Hello,
>
> A passkey named "Work laptop" was added to your Acme account on 19 November 2026 at
> 14:02 UTC, from Firefox on Linux. It can sign in to your account without a code.
>
> If you added it, there is nothing to do.
>
> If you didn't, sign in, remove it, and sign out your other devices:
> https://acme.example/account/settings

**Passkey removed.**

> **Subject:** A passkey was removed from your Acme account
>
> Hello,
>
> The passkey named "Work laptop" was removed from your Acme account on 19 November 2026
> at 14:02 UTC, from Firefox on Linux. It can no longer sign in.
>
> If you removed it, there is nothing to do.
>
> If you didn't, sign in and check your passkeys and devices:
> https://acme.example/account/settings

**Codes paused**, once per window, to an address with a user.

> **Subject:** Sign-in codes for your Acme account are paused
>
> Hello,
>
> Wrong sign-in codes for your Acme account were entered many times today. Until
> 20 November 2026 at 14:02 UTC, Acme only accepts codes for it on browsers where you
> signed in before. Nobody signed in with the wrong codes.
>
> You can also sign in with a passkey, if you added one. If you can't sign in and need
> to, reply to this email.

**Address changed by support**, to the old address, from `local set-email`.

> **Subject:** Your Acme address was changed by support
>
> Hello,
>
> Acme's support changed the address of your Acme account from ann@example.com to
> ann@example.org on 19 November 2026 at 14:02 UTC. Every device was signed out and every
> passkey removed. Sign-in codes now go to the new address.
>
> If you didn't ask for this, reply to this email right away, and quote this reference:
> 6f1c2e5a-0b7d-4c1e-9a53-2d8e4f7b1c90

**Address set by support**, to the new address, from `local set-email`.

> **Subject:** Your Acme account now uses this address
>
> Hello,
>
> Acme's support set this address on your Acme account on 19 November 2026 at 14:02 UTC.
> Every device was signed out and every passkey removed. To sign in, ask for a code with
> this address at acme.example, then add your passkeys again.
>
> If you didn't ask for this, reply to this email.

Several notices tell people to reply, so replies to the sending address must reach a
person (the sender's `ReplyTo`).

What a custom `Message` receives:

```go
type Kind int // SignInCode, SignUpCode, NewAddressCode, AddressTaken, AddressChanged,
              // PasskeyAdded, PasskeyRemoved, CodesPaused, AddressChangedBySupport,
              // AddressSetBySupport

type Notice struct {
	Kind        Kind
	To          string
	AppName     string
	Site        string    // the public URL's host: "acme.example"
	Code        string    // "4821 0937", in the code emails
	Minutes     int       // how long the code works
	OldEmail    string    // the address changes
	NewEmail    string    // the address changes
	Passkey     string    // the passkey's name
	Device      string    // the device that made the change: "Firefox on Linux"
	At          time.Time // when it happened
	Until       time.Time // CodesPaused: when codes work again everywhere
	Reference   string    // the user's ID, for support
	SettingsURL string
}
```

The email always goes to `Notice.To`, whatever the message's `To` says.

Sign-in now depends on these emails arriving. Each sending domain needs SPF, DKIM and
DMARC, and `docs/operations.md` must say so.

## Threats

These are the ways to take over, lock out or learn about an account, and what stops
each one.

| Threat | What stops it |
|---|---|
| Guessing a code | 8 digits; 5 wrong entries end a code; 20 an address a window, so at most 40 in any 24 hours; 100 a network an hour. The code is bound to its attempt cookie. |
| Using a code twice | It is deleted in the transaction that creates the session, under the user's and the code's row locks. |
| Reading codes from a copy of the database | Codes are stored as HMACs keyed with a secret the database doesn't hold. |
| An old code found in a mailbox later | Codes expire after 10 minutes, and a new code for the attempt ends the old one. |
| A code sent before a deletion, an address change or a recovery | A code sent to an existing user goes with that user, and works only while the user still has the address it went to. Address changes and recoveries delete the user's waiting codes. |
| Relaying a code through a phishing site | A code can be phished; a passkey can't, because the browser binds it to the real site. Code emails name the site. The settings page encourages adding a passkey after a code sign-in. |
| Learning whether an address has an account | One flow for sign-up and sign-in, with the same page and limits either way. An address change to a taken address makes a decoy attempt that behaves exactly like a real one. Both branches send an email, so timing matches too. |
| Flooding a mailbox through the app | Per-address, per-network, per-user and optional app-wide sending limits. |
| Locking someone out with wrong codes or by using up their codes | Only the shared counters can be used up, and they reset each window. Known browsers have their own counters, and passkeys always work. A code-only user on a new browser stays locked out while an attacker keeps at it: see [The limits](#the-limits). |
| Stealing a session cookie | `HttpOnly`, `Secure`, `__Host-`, `SameSite=Lax`. Never in a URL or a log. |
| A stolen session lasting | Sessions are kept on the server and checked on every request. They end after 30 idle days and 90 days in all. They can be signed out one by one, all at once, or by an operator. |
| Keeping a copied session through an address change | The change gives this browser a new token and ends every other session. |
| Session fixation | Every sign-in makes a new token and ends the browser's previous session. |
| Signing a victim in to the attacker's account (login CSRF) | The CSRF token and Origin check on every sign-in form. |
| A stolen or unattended session taking over for good | Adding or removing passkeys, changing the address and deleting need a sign-in within 5 minutes. Adding a passkey and changing the address check it again when they finish. |
| Changing the address to the attacker's | A recent sign-in, a code to the new address, a queued notice to the old one, and every other session ends. |
| The old mailbox signing up again after an address change | The account's primary address and the identity's recorded one change in the same transaction as the user's. A new user at the old address finds no account with it, and gets a new, empty one. |
| A request from before an address change writing the old address back | For a `TxProvider`, `RequireSignIn` never writes addresses. Only `AddressChanged` does. |
| A sign-in or passkey registration racing a recovery or deletion | Every such transaction locks the user's row first, and rechecks the code, passkey or session that authorised it. Whichever commits second sees the other's result. |
| Adding the attacker's passkey | A recent sign-in, checked again at the finish, and a queued notice to the address. |
| A copied passkey | If its authenticator keeps a counter, a counter that goes back stops sign-in with it, and is logged. Synced passkeys keep no counter, so this catches only some copies. |
| Replaying a passkey assertion | Single-use challenges that expire after 5 minutes, recorded in the transaction that uses them. |
| Finishing a ceremony in another context | The sealed ceremony data names its purpose, and for adding or confirming, the user and the session that started it. |
| Suppressing a security notice | Notices are queued in the change's transaction, sent with a context detached from the request, and retried for 3 days. |
| Redirecting after sign-in to another site | `next` must be a local path: one `/`, and no backslash or control character, which a browser would read as a separator after `http.Redirect` cleaned the path (`/../\evil.example` becomes `/\evil.example`). The same check guards Hanko's and `accounts`' paths. |
| A deleted account coming back | Deletion is one transaction with the user, and the user's codes go with it. Tombstones still stop in-flight requests. A new sign-up gets a new user ID, so a new, empty account. |
| Talking support into moving an account | `set-email` trusts the operator, and the runbook says what is never enough. It notifies both addresses, ends every session and removes every passkey, so nothing an attacker added survives a recovery. |
| Someone who controls the mailbox | They can sign in. The address is the root of trust by design: the notices tell the owner, and passkeys don't stop codes, because losing a passkey must not lock anyone out. |

## Configuration

```go
var Migrations = app.Migrations{Namespace: "local", Files: …}

type Config struct {
	AppName         string                     // in the emails: "Acme"
	PublicURL       string                     // as in web.Config: links, the emails' site, passkeys' origin
	Origins         []string                   // as in web.Config: further origins passkeys work on
	RPID            string                     // passkeys' domain; zero means PublicURL's host
	SecretKey       string                     // the app's secret key, as in web.Config
	Sender          email.Sender               // required: Scaleway's API, or SMTP
	SignedInPath    string                     // where sign-in goes without a next path; zero means "/"
	MaxCodesPerHour int                        // optional: codes the whole app may send an hour; zero means no ceiling
	Message         func(Notice) email.Message // optional: the app's own emails
}

func New(a *app.App, cfg Config) (*Local, error) // registers the commands and the jobs
func (l *Local) Mount(r gin.IRouter, layout ui.Layout) // /login and its steps under r
// *Local is an identity.TxProvider and accounts' profile UI.
```

`New` refuses a configuration that would break passkeys or the flows:
- `RPID` must be a bare host: lower case, with no scheme, port or path.
- The host of `PublicURL` and of every origin must be `RPID` or end in `.` + `RPID`.
- Origins use `https`, except loopback hosts in development.
- `Origins` must be the same list the app gives `web.Config`.

`RPID` must never change once passkeys exist, because every passkey belongs to one
domain. An app on `app.acme.example` that wants passkeys to work across `acme.example`
sets it to `acme.example`.

`SecretKey` is the same value the app gives `web.Config`. The module derives its own key
from it, and needs it outside requests too, in `local unblock`.

The wiring in a generated app:

```go
idp, err := local.New(a, local.Config{
	AppName:      "Acme",
	PublicURL:    cfg.PublicURL,
	SecretKey:    cfg.SecretKey,
	Sender:       mailer,
	SignedInPath: "/account",
})
accts, err := accounts.New(a, idp, accounts.Config{}) // lends idp its hooks
// routes.go
idp.Mount(r, layout)   // GET /login …
accts.Mount(r, layout) // the account pages; the settings page embeds the profile UI
```

Both jobs run under an advisory lock, so replicas don't repeat them.

**Logs** never contain addresses, codes, session tokens, challenges or IP addresses.
They carry user, session and passkey IDs. Address changes and recoveries are logged with
the user's ID and the time, so support can find them.

## Development and tests

**Email, in development and in production.** Sign-in needs email, so development needs
a mail server that works offline. A new adapter, `email/smtp`, is a real provider: it
requires STARTTLS unless the host is loopback. The generated `docker-compose.yml` runs
Mailpit beside PostgreSQL, and codes show up in Mailpit's web page.

Production sends through Scaleway's API with `email/scaleway`, as now, without SMTP. That
adapter also reports delivery, which `credits/expiry` needs. The generated `config.go`
builds one sender or the other:
- an SMTP URL (`ACME_SMTP_URL`, which the generated `Taskfile.yml` sets for development,
  to Mailpit) means SMTP;
- Scaleway's key and project (`ACME_SCALEWAY_SECRET_KEY`, `ACME_SCALEWAY_PROJECT_ID`)
  mean Scaleway's API;
- both, or neither, is a configuration error, reported at startup.

Either way, `ACME_EMAIL_FROM` is the sender's address.

**Tests.**
- **Integration tests** (`internal/testdb`) run the real flows against a fresh database:
  sign-up and sign-in with codes, every limit, known browsers, address changes with
  their decoys, recoveries, deletion in one transaction with `accounts`' hooks, the
  notice queue and the cleanup. A test sender in a `_test.go` file captures the emails.
- **Interleavings**, each with two transactions held open on purpose:
  - two submissions of one code;
  - a code sign-in or a passkey sign-in against a recovery, and against a deletion;
  - a passkey registration, a deletion and a device sign-out against a recovery;
  - a request that authenticated before an address change, finishing after it;
  - a code sent before an address change or a deletion, entered after it;
  - counters at the edge of a window.
- **Passkeys**: unit tests with a software authenticator in a `_test.go` file. They
  cover registration, sign-in, each case of the counter rule, a replayed challenge, a
  ceremony finished for another purpose, user or session, and two tabs. Textlab's
  browser test runs the pages in headless Chromium with its virtual authenticator, over
  the DevTools protocol.
- **`accounts`' tests** cover a `TxProvider`: both hooks inside the provider's
  transaction, a hook that fails and rolls everything back, and `RequireSignIn` never
  writing an address.

## Moving an app from Hanko

Whether Searchbase moves is decided later. It may stay on Hanko, which stays in C4Stack.
An app that does move goes like this:

1. Before the release, check that every address in `accounts.primary_email` is lower
   case. Accounts are joined by exact address, so a mixed-case one would get a second
   account instead. Lowercase any that aren't, if that collides with no other account.
2. The release switches the provider to `local`, with `RPID` set to the domain Hanko's
   passkeys use.
3. Each person signs in once with a code. Their new `local` identity joins their account
   by its verified address, and they add a passkey.
4. Whether Hanko's passkeys can be imported, instead of added again, is looked at when an
   app moves. It depends on what Hanko's export carries.
5. The Hanko users stay at Hanko until the tenant is deleted, once everyone has moved.
   Their identities stay in `account_identities`, unused.

## What changes elsewhere

- **`identity`**: the optional `TxProvider` interface and `AccountHooks`.
  `Identity.Method` says how the session signed in, `"code"` or `"passkey"`; it is empty
  from providers that don't say, such as Hanko.
- **`accounts`**:
  - `New` lends a `TxProvider` its hooks. `SignedIn` is `join` taking the provider's
    transaction. `AddressChanged` is `setEmail` taking the provider's transaction and
    refusing an address another account has.
  - For a `TxProvider`, `RequireSignIn` never updates addresses.
  - `Delete` and `deleteTx` delete a `TxProvider`'s users first, in the same
    transaction, and skip the deletion marks.
  - The delete page links to the provider's confirm page when it has one (a
    `ConfirmURL(next string) string` method), instead of signing out first.
  - The hooks' refusals wrap `identity.ErrRefused`, and an address another account has
    is `identity.ErrAddressTaken`.
- **`web`**: nothing. The provider names its session cookie by `web`'s rule itself
  (`__Host-` on HTTPS), from its own public URL, because `Authenticate` and `Logout` get
  only the request and the response writer.
- **`email/smtp`**: the new adapter.
- **The generator**: new apps use `identity/local`. `c4stack new --identity hanko`
  generates an app on Hanko instead, as today. With `local`, `config.go` drops the Hanko
  URL and reads the email settings (SMTP or Scaleway's API), and `docker-compose.yml`
  gains Mailpit.
- **`AGENTS.md`**: the public packages list. The identity rule's "Deletion goes to the
  provider first" gains: or, for a provider in the app's database, into the same
  transaction.
- **Docs**:
  - `architecture.md`: the package, its dependency on `app`, `TxProvider` and the
    single transactions. "Rate limiting belongs to the ingress" gains: except sign-in's
    counters, which the ingress cannot keep.
  - `guide.md`: the wiring.
  - `operations.md`:
    - an email sender for every app that signs people in, with SPF, DKIM and DMARC;
    - setting `TrustedProxies` correctly, and what goes wrong either way;
    - what rotating the secret key does, and rotating it on every replica together;
    - runbooks for `local unblock`, `local sign-out` and `local set-email`, including
      what is never enough to prove who is asking.

## Decisions

| Question | Decision | Reason |
|---|---|---|
| Codes or links | Codes | A code works when the email is read on another device. Mail scanners open links, which would use them up. |
| Code length | 8 digits | With the same limits, 6 digits give a 1 in 25,000 chance in any 24 hours for one address. 8 digits give 1 in 2,500,000. Phones fill them from the email either way. |
| Separate sign-up and sign-in | One flow | Two flows would tell anyone which addresses have accounts. |
| Sessions | On the server | Revoked at once, like Hanko's validation on every request. A token that validates itself cannot be signed out. |
| Where limits live | PostgreSQL | They count addresses, which the ingress never sees, and replicas must share them. |
| Security values | Fixed | One reviewed set. An app cannot weaken it by accident. |
| Changes that touch accounts | One transaction, through `TxProvider` | Sign-up, address changes and deletion can't be left half done, and no stale request can write an old address back. |
| Concurrent changes to one user | The user's row lock first, then a recheck | A sign-in, registration or change that raced a recovery or deletion fails, instead of surviving it. |
| After an address change | This browser stays signed in with a new token; every other session ends | It is recent and has just proved the new address, and a copy of its old token stops working. Hanko signed everyone out because it had to. |
| Lockout by wrong codes | Known browsers keep their own counters | The address's counters must be shared to stop guessing, so an attacker can use them up. Browsers that signed in before shouldn't depend on them. |
| Address changes to a taken address | A decoy attempt | Otherwise the attempt's behaviour would tell a signed-in person which addresses have accounts. |
| Codes when a passkey exists | Still accepted | Losing a passkey must not lock anyone out. The address stays the root of trust. |
| Security notices | Queued with the change | A notice committed with its change can't be lost to a crash or a closed tab. |
| A deleted user's queued notices | Still sent, then deleted: once the email provider accepts them, and after 3 days at most | They stay out of the deletion transaction, so deleting the account can't suppress the notice of an address change made just before. They're still deleted, only a little later: usually they were sent before the deletion, and a queued one never stays more than 3 days. |
| "New sign-in" emails | No | A code sign-in already needs the mailbox. Passkey sign-ins would make them noise that people learn to ignore. |
| Passkey ceremony state | Sealed cookie with the whole session data and purpose, plus a record of used challenges | No rows for visitors who only open the page. Single use where it matters, in the same transaction as what it produces. |
| User verification | Required | A passkey is the only factor, so the device's own check is the second one. |
| IP addresses | Never stored | Device labels let people recognise their sessions. Counters keep HMACs. |
| Names | `identity/local`, provider `local` | It says where the users live, as `hanko` says who keeps them. The provider name is stored with every identity and never changes. |
| Session lifetimes | 30 days idle, 90 days in all | Long enough for an app people visit rarely, short enough that a forgotten or stolen session ends. |
| Configurable limits | Only an optional ceiling for the whole app | The other values are security values, like `RecentSignIn`. A ceiling can only add a limit, and its right value depends on the app's size and sending domain. |
| Support recovery | `local set-email` | Someone without their mailbox or a passkey needs a way back. It removes sessions and passkeys, so an attacker's don't survive, and notifies both addresses. |
| How a session signed in | `Identity.Method` | An admin area can ask for a passkey, and an OIDC provider can report `amr`. It is an additive field. |
| The generator's default | `identity/local`, with `--identity hanko` | It needs only an email sender, and development works offline. A flag, not a prompt, keeps the generator working in scripts and tests. |
| Email in development | SMTP to Mailpit, Scaleway's API in production | Development runs the real flow offline. Production keeps the adapter that reports delivery. |
| An "account deleted" email | Not now | `accounts` owns deletion, so it would be `accounts`' email, not this module's. |
