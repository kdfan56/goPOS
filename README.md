# goPOS

A point-of-sale and inventory system for a mid-sized grocery store in Pakistan,
built to replace an ageing iPOS.net / SQL Server setup. Single Go binary, SQLite,
server-rendered HTML — three in-store terminals run it in a browser over the shop's
local WiFi, all hitting one server on the store PC.

## Stack

- **Backend:** Go, standard library only (`net/http` with `http.ServeMux`, `html/template`) — no web framework.
- **Database:** SQLite via `modernc.org/sqlite` (pure-Go, no cgo), raw SQL, no ORM. Every write uses `_txlock=immediate`.
- **Frontend:** HTML served directly from Go templates, vanilla JS only where the UI must be live (cart totals, barcode input).
- **Barcode:** USB scanner at the counter (acts as a keyboard); phone camera on the mobile page via ZXing.

## What it does

- **POS terminal** — scan to build a cart, record the sale in one atomic transaction, print a receipt. Payment method (cash/card) is recorded; card data is never touched (a separate physical terminal handles the card).
- **Returns** — look up a past sale by receipt number, pick lines and quantities, restock only sellable items, print a return slip.
- **Mobile scan page** (`/scan`) — phone camera looks up a product's name, price, and stock, and shows the last few stock additions so a receiver can tell if a pile is already counted.
- **Inventory** — stock in/out with a full `stock_movements` audit trail (every change logged with delta, reason, timestamp), low-stock/reorder lists, per-SKU reorder levels.
- **Receiving sessions** — start a labelled session ("Truck 1"), scan items into it with a running tally, finalize to commit all quantities atomically; other terminals see active sessions so two shipments of the same product can't collide.
- **Reporting** — sales by day (with hour-of-day breakdown and CSV export), per-product drill-down, and margin reports by category, supplier, and SKU using cost-at-sale snapshots.
- **Suppliers** — supplier records linked to products and purchase reporting.

## Roles

Two credential pairs, enforced by HTTP Basic Auth wrapping every route:

- **Admin** — everything.
- **Cashier** — only `/pos`, `/price-check`, `/pos/scan`, `/pos/checkout`, and receipts.

## Running it

```bash
go build -o goPOS .

# self-signed TLS cert (first run only)
openssl req -x509 -newkey rsa:2048 -nodes -keyout key.pem -out cert.pem \
  -days 365 -subj "/CN=localhost"

# leading space keeps credentials out of shell history
 GOPOS_USER=admin GOPOS_PASS=… GOPOS_CASHIER=cashier GOPOS_CASHIER_PASS=… ./goPOS
```

The server listens on `:8443` over TLS. Templates are parsed once at startup, so
restart after editing any `.html`. The database is `pos.db` (gitignored); delete it
and restart to reset — the seed re-runs.
