# Shopify Discount Archive

A small Go tool that ingests Shopify **Discounts export** CSVs, archives each one
as a timestamped *snapshot*, and tracks how every discount code's usage changes
over time. It ships with a command-line importer/reporter and a local web
dashboard.

Each CSV you export from Shopify is a point-in-time picture. Drop them in over
weeks or months and this tool builds the history Shopify itself doesn't keep:
uses-since-last-export per code, which codes are new, which were deleted, and
the full usage trend for any single code.

> **Privacy:** the database and the raw CSVs live under `data/`, which is
> git-ignored. Discount/customer data never leaves your machine and is never
> committed to this public repo.

## What it captures

From each export it stores every column Shopify provides (value, value type,
type, discount class, combine rules, customer selection, status, usage limit,
start/end, **times used in total**, …) and derives:

- **Usage delta** — uses gained per code since the previous snapshot
- **New codes** — codes that appeared since the previous snapshot
- **Removed codes** — codes deleted in Shopify between exports
- **Per-code history** — the full times-used trend, with an inline sparkline
- **Status changes** — active vs. expired over time

Imports are **idempotent**: a file is hashed (SHA-256) on the way in, so
re-importing the same export is a no-op and never double-counts.

## Install / build

Requires [Go](https://go.dev/dl/) 1.26+.

```sh
git clone https://github.com/OptimumMeans/ShopifyDiscount.git
cd ShopifyDiscount
go build -o shopifydiscount.exe .
```

## Usage

Export your discounts from Shopify (Discounts → Export → CSV), then:

```sh
# Import an export as a snapshot (flags must come before the file path)
shopifydiscount import discounts_export_1.csv

# OR pull straight from the Shopify Admin API (no manual export — see setup below)
shopifydiscount pull

# Print a text summary of the latest snapshot + top movers
shopifydiscount report

# Launch the dashboard at http://127.0.0.1:8080
shopifydiscount serve
```

## Pulling from the Shopify Admin API

`pull` fetches discounts directly via the Admin GraphQL API and stores a snapshot
— same shape as a CSV import, so the two are interchangeable and dedupe together.
An unchanged pull is a no-op (it hashes the fetched data).

**One-time setup — create an access token:**

1. In Shopify admin: **Settings → Apps and sales channels → Develop apps → Create an app**.
2. Under **Configuration → Admin API integration**, add the scope **`read_discounts`**.
3. **Install** the app, then copy the **Admin API access token** (starts with `shpat_`).

If your app was created in the Partner Dashboard / Shopify CLI, it has a
**client id + client secret** instead of a "reveal token" button. Mint a token
with the built-in OAuth flow — no Vercel/Neon needed:

```sh
# 1. In the app config, add http://localhost:3456/auth/callback as an allowed
#    redirect URL, and ensure read_discounts is in the app's scopes.
# 2. Run auth (opens your browser to approve), which saves the token:
shopifydiscount auth -client-id <CLIENT_ID> -client-secret <CLIENT_SECRET>
# 3. Pull:
shopifydiscount pull
```

**Or, if you already minted a token** (Admin "Develop apps" flow, or you have one
from another installed app with `read_discounts`/`write_discounts`), provide it
directly (checked in this order — flags, then env, then config file):

```sh
# a) flags
shopifydiscount pull -shop your-store.myshopify.com -token shpat_xxx

# b) environment variables
$env:SHOPIFY_SHOP="your-store.myshopify.com"   # PowerShell
$env:SHOPIFY_ADMIN_TOKEN="shpat_xxx"
shopifydiscount pull

# c) git-ignored config file at data/shopify.json
#    { "shop": "your-store.myshopify.com", "token": "shpat_xxx", "apiVersion": "2025-10" }
shopifydiscount pull
```

The token is **never** committed: `data/`, `shopify.json`, and `.env` are all
git-ignored. Set the shop via `-shop`, `SHOPIFY_SHOP`, or `data/shopify.json`.

**Bulk-code discounts** (one discount holding many unique codes, e.g. an
event's 200+ codes) are expanded to **one row per code**, each with its own
usage count, so individual codes can be tracked. Single-code and automatic
discounts remain one row keyed by the discount title (matching the CSV).

### Snapshot timing

The "taken at" time defaults to the CSV file's last-modified time (a good proxy
for when you exported it). Override it when importing older files:

```sh
shopifydiscount import -as-of 2026-06-01 discounts_export_june.csv
shopifydiscount import -as-of 2026-06-01T09:30:00-05:00 export.csv
```

### Flags

| Command  | Flag          | Default            | Meaning                                          |
| -------- | ------------- | ------------------ | ------------------------------------------------ |
| all      | `-data DIR`   | `data`             | Directory for the SQLite DB + archived CSVs      |
| `import` | `-as-of TIME` | file's mtime       | Snapshot time (`RFC3339` or `2006-01-02`)        |
| `import` | `-no-archive` | off                | Skip copying the raw CSV into `data/archive/`    |
| `serve`  | `-addr H:P`   | `127.0.0.1:8080`   | Listen address for the dashboard                 |

## How it works

- **Storage** — a single SQLite file (`data/discounts.db`) via the pure-Go
  `modernc.org/sqlite` driver, so there's no C toolchain to install.
- **Archive** — every imported CSV is copied verbatim to
  `data/archive/<timestamp>_<name>.csv` for safekeeping.
- **Web** — `net/http` with `html/template`; templates and CSS are embedded in
  the binary, so the single `.exe` is fully self-contained.

```
.
├── main.go                  # CLI: import / pull / serve / report
├── internal/
│   ├── csvimport/           # Shopify CSV parsing (header-mapped, BOM-safe)
│   ├── shopify/             # Admin GraphQL API client + discount mapping
│   ├── store/               # SQLite schema + snapshot/delta queries
│   └── web/                 # dashboard server, templates, static assets
└── data/                    # (git-ignored) database + archived CSVs
```

## Roadmap ideas

- CSV/JSON export of the computed deltas
- Per-owner rollups (group codes by owner)
- Scheduled pulls (Task Scheduler / cron) for automatic history
