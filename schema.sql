CREATE TABLE IF NOT EXISTS products (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    barcode TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    price_rupees INTEGER NOT NULL,
    stock INTEGER NOT NULL DEFAULT 0,
    cost_price_rupees INTEGER,
    category TEXT,
    supplier_id INTEGER REFERENCES suppliers(id),
    reorder_level INTEGER, -- NULL = use global default (lowStockThreshold in main.go)
    reorder_qty INTEGER,   -- suggested order quantity, shown on the reorder list

    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_products_barcode ON products(barcode);

-- kind='return' rows carry a NEGATIVE total_rupees and point at the sale they reverse
-- (decision 19 sign convention, decision 22 flow shape).
CREATE TABLE IF NOT EXISTS transactions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    total_rupees INTEGER NOT NULL,
    payment_method TEXT NOT NULL CHECK(payment_method IN ('cash','card')),
    kind TEXT NOT NULL DEFAULT 'sale' CHECK(kind IN ('sale','return')),
    original_transaction_id INTEGER REFERENCES transactions(id),
    -- Which till rang this up. Self-reported by the terminal, NULL when unset
    -- (decision 25). A reconciliation aid, not a security control.
    station TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Supports every dashboard and report predicate of the form
-- date(created_at, '+5 hours') = ?. That expression cannot use the index by
-- itself, but a range scan on created_at can, and the planner uses it for the
-- BETWEEN forms. Measured need: at 55k rows the dashboard answers in 0.22s.
CREATE INDEX IF NOT EXISTS idx_transactions_created ON transactions(created_at);

CREATE TABLE IF NOT EXISTS transaction_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    transaction_id INTEGER NOT NULL REFERENCES transactions(id),
    product_id INTEGER NOT NULL REFERENCES products(id),
    quantity INTEGER NOT NULL,
    unit_price_rupees INTEGER NOT NULL,
    line_total_rupees INTEGER NOT NULL,
    cost_price_at_sale_rupees INTEGER -- per-unit cost snapshot at checkout; NULL = unknown (pre-snapshot rows or product had no cost)
);

CREATE INDEX IF NOT EXISTS idx_items_transaction ON transaction_items(transaction_id);

-- Serves every query that filters line items by product: the per-product
-- drill-down report /reports/product/{id} above all. Measured on 232,832 line
-- items: 0.002s with this index, 0.016s without, because the alternative is a
-- full scan of the table for one product. The gap grows with the row count, and
-- Phase G multiplies that count by about 20.
CREATE INDEX IF NOT EXISTS idx_items_product ON transaction_items(product_id);

-- A line the cashier pulled off the cart BEFORE the sale completed (decision 27).
-- A void is not a return: no money moved and the goods never left the shelf, so
-- a void row NEVER touches stock and NEVER reaches a money SUM. Keeping voids in
-- their own table is what guarantees that — do not store them as
-- transaction_items rows with a flag.
CREATE TABLE IF NOT EXISTS transaction_voids (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    -- NOT NULL on purpose. Every void belongs to a completed sale. A cart that
    -- is abandoned entirely never reaches the server, and decision 27 accepts
    -- that limit explicitly.
    transaction_id INTEGER NOT NULL REFERENCES transactions(id),
    product_id INTEGER NOT NULL REFERENCES products(id),
    quantity INTEGER NOT NULL,
    -- The price the customer was quoted, not the price of today (decision 18's rule).
    unit_price_rupees INTEGER NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_voids_transaction ON transaction_voids(transaction_id);
CREATE INDEX IF NOT EXISTS idx_voids_created ON transaction_voids(created_at);

-- Audit log: one row per stock change. reason: 'sale' | 'manual_adjust' | 'initial' | 'receiving'
CREATE TABLE IF NOT EXISTS stock_movements (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id INTEGER NOT NULL REFERENCES products(id),
    delta INTEGER NOT NULL,
    new_stock INTEGER NOT NULL,
    reason TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Composite index supports the "last N additions for this product" query on /scan.
CREATE INDEX IF NOT EXISTS idx_movements_product_created ON stock_movements(product_id, created_at DESC);

-- Suppliers: active=0 hides them from new-session dropdowns but keeps history.
CREATE TABLE IF NOT EXISTS suppliers (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT NOT NULL,
    phone          TEXT,
    contact_person TEXT,
    address        TEXT,
    active         INTEGER NOT NULL DEFAULT 1,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Receiving sessions: 'active' while scanning, 'finalized' once committed to stock.
CREATE TABLE IF NOT EXISTS receiving_sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    label        TEXT NOT NULL,
    supplier_id  INTEGER REFERENCES suppliers(id),
    status       TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','finalized')),
    started_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    finalized_at DATETIME
);

-- One row per product per session; re-scanning bumps qty (UNIQUE enforces it).
CREATE TABLE IF NOT EXISTS receiving_session_items (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES receiving_sessions(id),
    product_id INTEGER NOT NULL REFERENCES products(id),
    qty        INTEGER NOT NULL DEFAULT 0,
    UNIQUE(session_id, product_id)
);

-- Supports listing a session's items and the active-sessions view.
CREATE INDEX IF NOT EXISTS idx_session_items_session ON receiving_session_items(session_id);
