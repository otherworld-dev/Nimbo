# Nimbo business licences — operations runbook

**Internal.** How to issue and manage Nimbo business licences. Not for end users.

A Nimbo business licence is a self-contained, signed token. The app verifies it
**offline** against a public key baked into the build — there is no licence
server. That means:

- only the holder of the **private signing key** (you) can mint a valid licence;
- customers don't need to be online, and you don't run any infrastructure;
- a licence can't be revoked remotely — control it with **expiry dates**
  (issue 1-year licences and re-issue on renewal).

---

## ⚠️ The signing key is everything

The private key lives at **`tools/licensegen/.signing-key`** on the dev machine.
It is **gitignored and must never be committed**.

- **Back it up** somewhere safe and private (password manager / encrypted
  backup). It is 88 bytes of base64.
- **If you lose it:** you must `genkey` a new pair, paste the new public key into
  `internal/license/license.go`, ship a new build, and re-issue every customer's
  licence. Old licences stop validating against the new build.
- **If it leaks:** anyone can mint licences. Same recovery as "lost" — rotate the
  key and ship a build with the new public key.

The matching **public** key is embedded in `internal/license/license.go`
(`SigningPublicKey`). Changing it is a breaking change for existing licences.

---

## One-time setup (already done)

```sh
go run ./tools/licensegen genkey
```

Writes the private key to `tools/licensegen/.signing-key` and prints the
`SigningPublicKey` Go literal to paste into `internal/license/license.go`. It
refuses to overwrite an existing key (so you can't clobber it by accident).

---

## Issue a licence

```sh
# Perpetual licence:
go run ./tools/licensegen sign -customer "Acme Ltd" -seats 50

# Time-limited (recommended — renew yearly):
go run ./tools/licensegen sign -customer "Acme Ltd" -seats 50 -expires 2027-06-15
```

Flags:

| Flag | Meaning |
|---|---|
| `-customer` | Organisation name (shown in their app). **Required.** |
| `-seats` | Licensed seat count (record-keeping; `0` = unspecified). |
| `-expires` | `YYYY-MM-DD`; omit for perpetual. |
| `-tier` | Defaults to `business` (the only paid tier today). |
| `-id` | Licence id; auto-generated if omitted. |

It prints a token like `NIMBO-LIC-1.…`. **Email that token to the customer.**
Keep a record of who you issued which `id` to (a simple spreadsheet is fine).

---

## Customer activation

The customer pastes the token into **Settings → General → About → Licence →
Activate**. The app validates the signature and expiry locally and then shows
*"Business licence — Acme Ltd · 50 seats · expires 2027-06-15."* No internet
required.

To move to another machine they paste the same token again. To stop, **Remove
licence** reverts to free personal mode.

---

## Renewals & expiry

There's no remote kill switch by design. For control, issue **dated** licences
and re-issue on renewal (same command, later `-expires`). An expired licence
shows as expired in the app and stops unlocking business features, but never
disrupts ordinary syncing — the free personal tier always keeps working.

---

## How the gate works in code

`app.licensed(license.TierBusiness)` reports whether business features are
unlocked. **Free features never call it.** When you add a business-only feature
(central policy, white-label, …), guard it behind that check. See
`internal/license/license.go` and the memory note `nimbo-licensing-tech`.
