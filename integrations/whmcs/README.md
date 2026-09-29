# WPGenie module for WHMCS

A WHMCS server (provisioning) module: every WHMCS service is a WPGenie account on a WPGenie plan. WHMCS
creates, suspends, unsuspends and terminates the account, changes its plan, sets its user's password,
imports disk and bandwidth usage, and signs the client in to the WPGenie dashboard with one click.

## What you need

- WPGenie reachable over HTTPS on its panel domain (`PANEL_DOMAIN` at install), from the WHMCS server.
- Plans in WPGenie (`wpgenie plan create …` or the dashboard's *Plans* tab).
- An API token. Create a panel user for WHMCS and a token for it:

  ```bash
  wpgenie user add whmcs --role admin      # or use a reseller account's user: see below
  wpgenie token create --user whmcs --name WHMCS
  ```

  The token acts as that user (an administrator here). A token of a **reseller** account's user works
  too: WHMCS then creates customer accounts under that reseller, only with plans the reseller may
  hand out, and can't see or touch anything else. If the panel requires two-factor authentication, the
  token's user must have it enabled (sign in once as that user and enrol under Account).

  Service IDs are per billing system: the panel owner's WHMCS and a reseller's WHMCS can both have a
  service #123 without ever touching each other's accounts.

## Install

1. Copy `modules/servers/wpgenie/` into your WHMCS installation's `modules/servers/`.
2. *System Settings → Servers → Add New Server*:
   - Module: **WPGenie**
   - Hostname: your panel domain (e.g. `panel.example.com`)
   - Password (or Access Hash): the API token
   - Secure: on (the module only ever uses HTTPS and verifies the certificate)
   - *Test Connection* should succeed.
3. *System Settings → Products/Services*: create a product with module **WPGenie**, set **Plan** to a
   WPGenie plan ID and **Account type** to `customer` (or `reseller`).
4. Enable *Automation Settings → Update Usage Statistics* to import disk and bandwidth nightly.

## What each action does

| WHMCS | WPGenie |
|---|---|
| Create | `POST /api/v1/accounts` with the client's name and email, the service ID and a first user (the service username, else the client's email). Idempotent: the key `whmcs-<service id>` returns the existing account if WHMCS retries. |
| Suspend / Unsuspend | Suspends the account with reason `billing`: every site answers a static 503 page, PHP stops, nothing is deleted. |
| Terminate | Terminates the account and deletes its sites (their backups stay in their destinations). Run again to retry sites that failed. |
| Change Package | Changes the plan. |
| Change Password | Sets the user's password (WPGenie needs 12+ characters). |
| Usage Update | Disk (files + databases) and this month's bandwidth, in MB, with the plan's limits. |
| Single Sign-On | A one-time link valid for two minutes. Clients with two-factor authentication still enter their code. |

Passwords shorter than 12 characters aren't sent: WPGenie generates one and the client signs in through
WHMCS (single sign-on). API calls are recorded in WHMCS's module log with the token and passwords masked.

## Troubleshooting

- *No WPGenie account for service #N*: the account was never created (run Create) or was created
  outside WHMCS; set its WHMCS service ID in WPGenie (`PUT /api/v1/accounts/{id}` with
  `whmcs_service_id`).
- *plan … exceeds the reseller's plan* / *not offered to resellers*: with a reseller's token, the
  product's plan must be resellable and fit within the reseller's own plan.
