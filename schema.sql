CREATE TABLE IF NOT EXISTS products (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    barcode TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    price_rupees INTEGER NOT NULL,
    stock INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_products_barcode ON products(barcode);

CREATE TABLE IF NOT EXISTS transactions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    total_rupees INTEGER NOT NULL,
    payment_method TEXT NOT NULL CHECK(payment_method IN ('cash','card')),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS transaction_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    transaction_id INTEGER NOT NULL REFERENCES transactions(id),
    product_id INTEGER NOT NULL REFERENCES products(id),
    quantity INTEGER NOT NULL,
    unit_price_rupees INTEGER NOT NULL,
    line_total_rupees INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_items_transaction ON transaction_items(transaction_id);

-- Audit log: one row per stock-changing event. See decisions/09-receiving-workflow.md.
-- delta is signed (+ for additions, - for sales / adjust-down).
-- new_stock is the post-change snapshot — kept for sanity checks and debugging,
-- cheap to store and a future bug investigator will be glad it's here.
-- reason: 'sale' | 'manual_adjust' | 'initial' | 'receiving'
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

-- Suppliers: who we buy stock from. Minimal — name, contact details, active flag.
-- supplier_id is nullable on receiving_sessions so old sessions without a supplier
-- are not affected. active=0 hides a supplier from new-session dropdowns but keeps
-- them on historical sessions.
CREATE TABLE IF NOT EXISTS suppliers (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT NOT NULL,
    phone          TEXT,
    contact_person TEXT,
    address        TEXT,
    active         INTEGER NOT NULL DEFAULT 1,
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Receiving sessions (Phase 3.5b). See decisions/09-receiving-workflow.md and
-- decisions/12-receiving-sessions-schema.md.
-- A session is one shipment being scanned in. Person starts it with a label,
-- scans items into it, then finalizes — at which point every qty is committed to
-- products.stock atomically (one stock_movements row each, reason='receiving').
-- status: 'active' while scanning, 'finalized' after commit. An un-finalized
-- session can be deleted outright; a finalized one never can (it changed stock).
CREATE TABLE IF NOT EXISTS receiving_sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    label        TEXT NOT NULL,
    supplier_id  INTEGER REFERENCES suppliers(id),
    status       TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','finalized')),
    started_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    finalized_at DATETIME
);

-- One row per distinct product in a session. Re-scanning the same product bumps
-- qty rather than inserting a duplicate — enforced by the UNIQUE constraint.
CREATE TABLE IF NOT EXISTS receiving_session_items (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES receiving_sessions(id),
    product_id INTEGER NOT NULL REFERENCES products(id),
    qty        INTEGER NOT NULL DEFAULT 0,
    UNIQUE(session_id, product_id)
);

-- Supports listing a session's items and the active-sessions view.
CREATE INDEX IF NOT EXISTS idx_session_items_session ON receiving_session_items(session_id);
