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
