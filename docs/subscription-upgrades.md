# Experimental subscription upgrades

Codex2API can expose an admin-only, quote-first workflow for two individual
ChatGPT subscription transitions:

- Plus (`plus`) to Pro 5x (`chatgptprolite`, observed entitlement `prolite`)
- Pro 5x (`prolite`) to Pro 20x (`chatgptpro`, observed entitlement `pro`)

This integration uses undocumented ChatGPT web backend endpoints. It is not a
stable OpenAI API contract and may change without notice. It is disabled by
default and must not be used for automatic or batch upgrades.

## Enable the API

Set the following environment variable and restart Codex2API:

```dotenv
CODEX2API_SUBSCRIPTION_UPGRADES_ENABLED=true
```

All endpoints remain behind the existing admin authentication middleware.

## API flow

Read the current subscription:

```http
GET /api/admin/accounts/{id}/subscription
```

Create a short-lived preview:

```http
POST /api/admin/accounts/{id}/subscription/upgrade-quotes
Content-Type: application/json

{
  "target_plan": "chatgptpro",
  "currency": "PHP"
}
```

The response includes the exact amount currently due in minor units, recurring
price, tax, line items, renewal date, payment-method presence, and a two-minute
`quote_id`. `silent_reauthorization_available` reports whether Codex2API already
holds a separate Web Session that can be tried if the paid update invalidates
the current OAuth credential family. It never exposes that session credential.

After a human reviews that preview, submit a bounded confirmation:

```http
POST /api/admin/accounts/{id}/subscription/upgrades
Idempotency-Key: a-unique-value-for-this-one-upgrade
Content-Type: application/json

{
  "quote_id": "...",
  "currency": "PHP",
  "max_amount_minor": 350000,
  "confirmed": true
}
```

Immediately before the paid POST, Codex2API re-reads the current plan and
refreshes the upstream quote. It rejects the operation if the currency changes,
the plan changes, the account becomes delinquent, or the fresh amount is above
the confirmed cap.

Read the durable operation journal with:

```http
GET /api/admin/subscription-upgrades/{operation_id}
```

The journal stores the amount, currency, transition, sanitized status, and a
SHA-256 hash of the idempotency key. It never stores OAuth tokens, cookies,
payment method IDs, Stripe secrets, or card data.

## Payment and verification states

- `succeeded`: the paid request was accepted and the target entitlement was
  observed.
- `requires_user_action`: the upstream requested 3DS or another payment action;
  Codex2API stops and does not retry.
- `verification_pending`: the paid request was accepted, but the target
  entitlement is not visible yet. Reconcile with read-only requests; do not
  repeat payment.
- `verification_requires_reauthentication`: the paid request was accepted, but
  the OAuth credential family was subsequently rejected. Reauthorize the
  account, then verify the existing operation; do not repeat payment.
- `ambiguous_transport`: the connection failed after submission may have begun.
  Reconcile the subscription and billing state manually; do not retry the paid
  POST automatically.

An `Idempotency-Key` is unique per account. Repeating the same request returns
the existing operation and never sends a second paid POST.

## Login preservation

Saving and restoring the same access token or refresh token cannot avoid login
when the upstream invalidates that credential family server-side. Codex2API can
silently recover only when the account also has a separate, still-valid ChatGPT
Web Session. In that case it attempts one normal account refresh and repeats
only the read-only entitlement verification. It never repeats the paid POST.

If no valid Web Session exists, `verification_requires_reauthentication` is the
correct terminal state until an administrator logs the account in again.

## Sanitized canary record (2026-08-27)

A single confirmed Pro 5x to Pro 20x canary was performed before this feature
was implemented:

- localized recurring price: PHP 9,990/month, including 12% VAT;
- refreshed amount due: PHP 3,451.96;
- paid update endpoint returned HTTP 200 exactly once;
- the existing access and refresh tokens were rejected afterward;
- no separate Web Session was stored, so manual reauthentication was required;
- after reauthentication, the upstream subscription reported `plan_type=pro`.

This outcome is why an accepted payment followed by a 401 is represented as
`verification_requires_reauthentication`, not as payment failure.

## Upstream contract observed by the canary

```text
GET  /backend-api/subscriptions?account_id={workspace_id}
GET  /backend-api/subscriptions/update/preview?account_id={workspace_id}&updated_plan={plan}
GET  /backend-api/checkout_pricing_config/configs/{currency}
POST /backend-api/subscriptions/update
```

The generic `/backend-api/payments/checkout` endpoint is not used for upgrades
of an already-paid subscription.
