package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	_ "modernc.org/sqlite"
)

var storeLocation *time.Location

const lowStockThreshold = 10

type Product struct {
	ID           int
	Barcode      string
	Name         string
	PriceRupees  int
	Stock        int
	Category     string
	SupplierID   sql.NullInt64
	ReorderLevel sql.NullInt64
	ReorderQty   sql.NullInt64
	Threshold    int    // effective low-stock cutoff: COALESCE(reorder_level, lowStockThreshold)
	SupplierName string // filled by the products list query only
}

func main() {
	loc, err := time.LoadLocation("Asia/Karachi")
	if err != nil {
		log.Fatalf("load timezone: %v", err)
	}
	storeLocation = loc

	db, err := sql.Open("sqlite", "pos.db")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// SQLite ignores REFERENCES clauses unless this is on
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		log.Fatalf("enable foreign keys: %v", err)
	}

	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		log.Fatalf("read schema.sql: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		log.Fatalf("apply schema: %v", err)
	}

	// no ADD COLUMN IF NOT EXISTS in SQLite — ignore duplicate-column on rerun
	if _, err := db.Exec(`ALTER TABLE receiving_sessions ADD COLUMN supplier_id INTEGER REFERENCES suppliers(id)`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			log.Fatalf("migrate receiving_sessions.supplier_id: %v", err)
		}
	}
	for _, col := range []string{
		`ALTER TABLE products ADD COLUMN cost_price_rupees INTEGER`,
		`ALTER TABLE products ADD COLUMN category TEXT`,
		`ALTER TABLE products ADD COLUMN supplier_id INTEGER REFERENCES suppliers(id)`,
		`ALTER TABLE products ADD COLUMN reorder_level INTEGER`,
		`ALTER TABLE products ADD COLUMN reorder_qty INTEGER`,
		`ALTER TABLE transaction_items ADD COLUMN cost_price_at_sale_rupees INTEGER`,
	} {
		if _, err := db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			log.Fatalf("migrate: %v", err)
		}
	}

	if err := seedIfEmpty(db); err != nil {
		log.Fatalf("seed: %v", err)
	}

	tmpl, err := template.ParseGlob("templates/*.html")
	if err != nil {
		log.Fatalf("parse templates: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", dashboardHandler(db, tmpl))
	mux.HandleFunc("GET /products", productsHandler(db, tmpl))
	mux.HandleFunc("GET /products/new", newProductFormHandler(tmpl))
	mux.HandleFunc("POST /products/new", createProductHandler(db, tmpl))
	mux.HandleFunc("GET /products/{id}/edit", editProductFormHandler(db, tmpl))
	mux.HandleFunc("POST /products/{id}/edit", editProductHandler(db, tmpl))
	mux.HandleFunc("GET /pos", posHandler(tmpl))
	mux.HandleFunc("GET /price-check", priceCheckPageHandler(tmpl))
	mux.HandleFunc("POST /pos/scan", scanHandler(db))
	mux.HandleFunc("GET /scan", scanPageHandler(tmpl))
	mux.HandleFunc("POST /pos/checkout", checkoutHandler(db))
	mux.HandleFunc("GET /receipt/{id}", receiptHandler(db, tmpl))
	mux.HandleFunc("POST /stock/update", stockUpdateHandler(db))
	mux.HandleFunc("GET /reports", reportsHandler(db, tmpl))
	mux.HandleFunc("GET /reports/csv", reportsCSVHandler(db))
	mux.HandleFunc("GET /reports/product/{id}", productDrilldownHandler(db, tmpl))
	mux.HandleFunc("GET /reports/suppliers", reportsSuppliersHandler(db, tmpl))
	mux.HandleFunc("GET /reports/categories", reportsCategoriesHandler(db, tmpl))
	mux.HandleFunc("GET /reports/itemwise", reportsItemwiseHandler(db, tmpl))
	mux.HandleFunc("GET /reports/itemwise/csv", reportsItemwiseCSVHandler(db))
	mux.HandleFunc("GET /receiving", receivingListHandler(db, tmpl))
	mux.HandleFunc("POST /receiving/new", createReceivingSessionHandler(db, tmpl))
	mux.HandleFunc("GET /receiving/{id}", receivingDetailHandler(db, tmpl))
	mux.HandleFunc("POST /receiving/{id}/scan", receivingScanHandler(db))
	mux.HandleFunc("POST /receiving/{id}/item", receivingItemHandler(db))
	mux.HandleFunc("POST /receiving/{id}/delete", receivingDeleteHandler(db))
	mux.HandleFunc("POST /receiving/{id}/finalize", receivingFinalizeHandler(db))
	mux.HandleFunc("GET /suppliers", suppliersHandler(db, tmpl))
	mux.HandleFunc("GET /suppliers/new", newSupplierFormHandler(tmpl))
	mux.HandleFunc("POST /suppliers/new", createSupplierHandler(db, tmpl))
	mux.HandleFunc("GET /suppliers/{id}/edit", editSupplierFormHandler(db, tmpl))
	mux.HandleFunc("POST /suppliers/{id}/edit", updateSupplierHandler(db, tmpl))

	adminUser := os.Getenv("GOPOS_USER")
	adminPass := os.Getenv("GOPOS_PASS")
	if adminUser == "" || adminPass == "" {
		log.Fatal("GOPOS_USER and GOPOS_PASS env vars must be set before starting the server")
	}
	cashierUser := os.Getenv("GOPOS_CASHIER")
	cashierPass := os.Getenv("GOPOS_CASHIER_PASS")
	if cashierUser == "" || cashierPass == "" {
		log.Fatal("GOPOS_CASHIER and GOPOS_CASHIER_PASS env vars must be set before starting the server")
	}
	handler := requireAuth(adminUser, adminPass, cashierUser, cashierPass, mux)

	addr := ":8443"
	log.Printf("listening on %s (TLS)", addr)
	if err := http.ListenAndServeTLS(addr, "cert.pem", "key.pem", handler); err != nil {
		log.Fatal(err)
	}
}

type ctxKey int

const roleKey ctxKey = iota

func requireAuth(adminUser, adminPass, cashierUser, cashierPass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="goPOS"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}

		role := ""
		if credMatch(u, p, adminUser, adminPass) {
			role = "admin"
		} else if credMatch(u, p, cashierUser, cashierPass) {
			role = "cashier"
		}
		if role == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="goPOS"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}

		if role == "cashier" && !cashierAllowed(r.URL.Path) {
			http.Error(w, "forbidden — cashier access is limited to POS and price check", http.StatusForbidden)
			return
		}

		ctx := context.WithValue(r.Context(), roleKey, role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// constant-time so response timing can't leak credentials
func credMatch(u, p, wantUser, wantPass string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(u), []byte(wantUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(p), []byte(wantPass)) == 1
	return userOK && passOK
}

// anything not listed here is admin-only
func cashierAllowed(path string) bool {
	switch path {
	case "/", "/pos", "/price-check", "/pos/scan", "/pos/checkout":
		return true
	}

	return strings.HasPrefix(path, "/receipt/")
}

func seedIfEmpty(db *sql.DB) error {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM products").Scan(&count); err != nil {
		return fmt.Errorf("count products: %w", err)
	}
	if count > 0 {
		return nil
	}

	dummies := []Product{
		{Barcode: "8964000123456", Name: "Olper's Milk 1L", PriceRupees: 290, Stock: 40},
		{Barcode: "8964000234567", Name: "Dawn Bread Large", PriceRupees: 180, Stock: 25},
		{Barcode: "8964000345678", Name: "Tapal Danedar 475g", PriceRupees: 950, Stock: 30},
		{Barcode: "8964000456789", Name: "National Salt 800g", PriceRupees: 120, Stock: 60},
		{Barcode: "8964000567890", Name: "Sufi Cooking Oil 1L Pouch", PriceRupees: 620, Stock: 35},
		{Barcode: "8964000678901", Name: "Lays Masala 40g", PriceRupees: 60, Stock: 80},
		{Barcode: "8964000789012", Name: "Coca-Cola 1.5L", PriceRupees: 250, Stock: 50},
		{Barcode: "8964000890123", Name: "Knorr Chicken Cube 4pc", PriceRupees: 90, Stock: 45},
	}

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed tx: %w", err)
	}
	defer tx.Rollback()

	for _, p := range dummies {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO products (barcode, name, price_rupees, stock) VALUES (?, ?, ?, ?)",
			p.Barcode, p.Name, p.PriceRupees, p.Stock,
		)
		if err != nil {
			return fmt.Errorf("insert %s: %w", p.Barcode, err)
		}
		productID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("last insert id %s: %w", p.Barcode, err)
		}
		if err := recordMovement(ctx, tx, int(productID), p.Stock, p.Stock, "initial"); err != nil {
			return fmt.Errorf("record initial movement %s: %w", p.Barcode, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed: %w", err)
	}
	log.Printf("seeded %d dummy products", len(dummies))
	return nil
}

func posHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := tmpl.ExecuteTemplate(w, "pos.html", nil); err != nil {
			log.Printf("render pos: %v", err)
		}
	}
}

func priceCheckPageHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := tmpl.ExecuteTemplate(w, "price-check.html", nil); err != nil {
			log.Printf("render price-check: %v", err)
		}
	}
}

func scanPageHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := tmpl.ExecuteTemplate(w, "scan.html", nil); err != nil {
			log.Printf("render scan: %v", err)
		}
	}
}

func scanHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Barcode string `json:"barcode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad request")
			return
		}

		req.Barcode = normalizeBarcode(req.Barcode)
		if req.Barcode == "" {
			writeJSONError(w, http.StatusBadRequest, "barcode required")
			return
		}

		// Match either the UPC-A or EAN-13 form: an imported 12-digit product is
		// found whether scanned as 12 or 13 digits. alt falls back to the same
		// value when there's no equivalent, so the OR is harmless.
		alt := altBarcode(req.Barcode)
		if alt == "" {
			alt = req.Barcode
		}
		var p Product
		err := db.QueryRow(
			"SELECT id, barcode, name, price_rupees, stock, COALESCE(category,''), COALESCE(reorder_level, ?) FROM products WHERE barcode = ? OR barcode = ?",
			lowStockThreshold, req.Barcode, alt,
		).Scan(&p.ID, &p.Barcode, &p.Name, &p.PriceRupees, &p.Stock, &p.Category, &p.Threshold)

		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "barcode not found")
			return
		}
		if err != nil {
			log.Printf("scan query: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "lookup failed")
			return
		}

		additions := recentAdditions(db, p.ID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":               p.ID,
			"barcode":          p.Barcode,
			"name":             p.Name,
			"price_rupees":     p.PriceRupees,
			"stock":            p.Stock,
			"category":         p.Category,
			"low_threshold":    p.Threshold,
			"recent_additions": additions,
		})
	}
}

type recentAddition struct {
	Delta     int    `json:"delta"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
}

func recentAdditions(db *sql.DB, productID int) []recentAddition {
	out := []recentAddition{}
	rows, err := db.Query(

		`SELECT delta, reason, created_at
		 FROM stock_movements
		 WHERE product_id = ? AND delta > 0
		 ORDER BY created_at DESC, id DESC
		 LIMIT 5`,
		productID,
	)
	if err != nil {

		log.Printf("recent additions query: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var m recentAddition
		var ts time.Time
		if err := rows.Scan(&m.Delta, &m.Reason, &ts); err != nil {
			log.Printf("recent addition scan: %v", err)
			continue
		}
		m.CreatedAt = ts.In(storeLocation).Format("2 Jan, 3:04 PM")
		out = append(out, m)
	}
	return out
}

type checkoutItem struct {
	ProductID int `json:"product_id"`
	Quantity  int `json:"quantity"`
}

type checkoutRequest struct {
	PaymentMethod string         `json:"payment_method"`
	Items         []checkoutItem `json:"items"`
}

func checkoutHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req checkoutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad request")
			return
		}
		if req.PaymentMethod != "cash" && req.PaymentMethod != "card" {
			writeJSONError(w, http.StatusBadRequest, "payment_method must be 'cash' or 'card'")
			return
		}
		if len(req.Items) == 0 {
			writeJSONError(w, http.StatusBadRequest, "cart empty")
			return
		}

		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			log.Printf("begin tx: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "checkout failed")
			return
		}
		defer tx.Rollback()

		type priced struct {
			id          int
			name        string
			price       int
			cost        sql.NullInt64 // per-unit cost snapshot; NULL when product has no cost
			quantity    int
			stockBefore int
		}
		lines := make([]priced, 0, len(req.Items))
		total := 0

		for _, item := range req.Items {
			if item.Quantity <= 0 {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("quantity for product %d must be > 0", item.ProductID))
				return
			}
			var p priced
			p.id = item.ProductID
			p.quantity = item.Quantity
			err := tx.QueryRowContext(ctx,
				"SELECT name, price_rupees, stock, cost_price_rupees FROM products WHERE id = ?",
				item.ProductID,
			).Scan(&p.name, &p.price, &p.stockBefore, &p.cost)
			if errors.Is(err, sql.ErrNoRows) {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("product %d not found", item.ProductID))
				return
			}
			if err != nil {
				log.Printf("product lookup: %v", err)
				writeJSONError(w, http.StatusInternalServerError, "checkout failed")
				return
			}
			if p.stockBefore < item.Quantity {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("not enough stock for %s (have %d, need %d)", p.name, p.stockBefore, item.Quantity))
				return
			}
			total += p.price * p.quantity
			lines = append(lines, p)
		}

		res, err := tx.ExecContext(ctx,
			"INSERT INTO transactions (total_rupees, payment_method) VALUES (?, ?)",
			total, req.PaymentMethod,
		)
		if err != nil {
			log.Printf("insert transaction: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "checkout failed")
			return
		}
		txnID, err := res.LastInsertId()
		if err != nil {
			log.Printf("last insert id: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "checkout failed")
			return
		}

		for _, p := range lines {
			_, err := tx.ExecContext(ctx,
				"INSERT INTO transaction_items (transaction_id, product_id, quantity, unit_price_rupees, line_total_rupees, cost_price_at_sale_rupees) VALUES (?, ?, ?, ?, ?, ?)",
				txnID, p.id, p.quantity, p.price, p.price*p.quantity, p.cost,
			)
			if err != nil {
				log.Printf("insert item: %v", err)
				writeJSONError(w, http.StatusInternalServerError, "checkout failed")
				return
			}
			_, err = tx.ExecContext(ctx,
				"UPDATE products SET stock = stock - ? WHERE id = ?",
				p.quantity, p.id,
			)
			if err != nil {
				log.Printf("update stock: %v", err)
				writeJSONError(w, http.StatusInternalServerError, "checkout failed")
				return
			}
			if err := recordMovement(ctx, tx, p.id, -p.quantity, p.stockBefore-p.quantity, "sale"); err != nil {
				log.Printf("record sale movement: %v", err)
				writeJSONError(w, http.StatusInternalServerError, "checkout failed")
				return
			}
		}

		if err := tx.Commit(); err != nil {
			log.Printf("commit: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "checkout failed")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"transaction_id": txnID})
	}
}

type ReceiptItem struct {
	Name            string
	Quantity        int
	UnitPriceRupees int
	LineTotalRupees int
}

type Receipt struct {
	TransactionID int
	CreatedAt     string
	PaymentMethod string
	TotalRupees   int
	ItemCount     int
	Items         []ReceiptItem
}

func receiptHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}

		rcpt := Receipt{TransactionID: id}
		var createdAt time.Time
		err = db.QueryRow(
			"SELECT total_rupees, payment_method, created_at FROM transactions WHERE id = ?",
			id,
		).Scan(&rcpt.TotalRupees, &rcpt.PaymentMethod, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "receipt not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("receipt query: %v", err)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		rcpt.CreatedAt = createdAt.In(storeLocation).Format("2 Jan 2006, 3:04 PM")

		rows, err := db.Query(
			`SELECT p.name, ti.quantity, ti.unit_price_rupees, ti.line_total_rupees
			 FROM transaction_items ti
			 JOIN products p ON p.id = ti.product_id
			 WHERE ti.transaction_id = ?
			 ORDER BY ti.id`,
			id,
		)
		if err != nil {
			log.Printf("items query: %v", err)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var it ReceiptItem
			if err := rows.Scan(&it.Name, &it.Quantity, &it.UnitPriceRupees, &it.LineTotalRupees); err != nil {
				log.Printf("item scan: %v", err)
				http.Error(w, "lookup failed", http.StatusInternalServerError)
				return
			}
			rcpt.Items = append(rcpt.Items, it)
			rcpt.ItemCount += it.Quantity
		}

		if err := tmpl.ExecuteTemplate(w, "receipt.html", rcpt); err != nil {
			log.Printf("render receipt: %v", err)
		}
	}
}

type DailySales struct {
	Date        string
	Display     string
	IsToday     bool
	CashRupees  int
	CardRupees  int
	TotalRupees int
	TxnCount    int
}

type HourSales struct {
	Hour        int
	Display     string
	CashRupees  int
	CardRupees  int
	TotalRupees int
	TxnCount    int
}

type ReportsPageData struct {
	Days       []DailySales
	GrandCash  int
	GrandCard  int
	GrandTotal int
	GrandCount int
	FromDate   string
	ToDate     string
	HourRows   []HourSales
}

type productDaySales struct {
	Display   string
	Qty       int
	RevenueRs string
}

type productDrilldownData struct {
	ProductName  string
	FromDate     string
	ToDate       string
	Days         []productDaySales
	GrandQty     int
	GrandRevenue string
}

type CategorySales struct {
	Category  string
	Qty       int
	RevenueRs int
	CostRs    int
	ProfitRs  int
}

type reportsCategoriesPageData struct {
	Rows        []CategorySales
	FromDate    string
	ToDate      string
	GrandQty    int
	GrandRev    int
	GrandCost   int
	GrandProfit int
}

func parseDateRange(r *http.Request) (from, to time.Time) {
	nowLocal := time.Now().In(storeLocation)
	to = time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, storeLocation)
	from = to.AddDate(0, 0, -6)
	if s := r.URL.Query().Get("from"); s != "" {
		if t, err := time.ParseInLocation("2006-01-02", s, storeLocation); err == nil {
			from = t
		}
	}
	if s := r.URL.Query().Get("to"); s != "" {
		if t, err := time.ParseInLocation("2006-01-02", s, storeLocation); err == nil {
			to = t
		}
	}
	if from.After(to) {
		from, to = to, from
	}
	if days := int(to.Sub(from).Hours()/24) + 1; days > 90 {
		from = to.AddDate(0, 0, -89)
	}
	return
}

func buildDayScaffold(from, to time.Time) ([]string, map[string]*DailySales) {
	nowLocal := time.Now().In(storeLocation)
	todayISO := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, storeLocation).Format("2006-01-02")

	numDays := int(to.Sub(from).Hours()/24) + 1
	ordered := make([]string, 0, numDays)
	m := make(map[string]*DailySales, numDays)
	for i := 0; i < numDays; i++ {
		d := to.AddDate(0, 0, -i)
		iso := d.Format("2006-01-02")
		ordered = append(ordered, iso)
		var display string
		switch iso {
		case todayISO:
			display = "Today (" + d.Format("Mon 2 Jan") + ")"
		case time.Now().In(storeLocation).AddDate(0, 0, -1).Format("2006-01-02"):
			display = "Yesterday (" + d.Format("Mon 2 Jan") + ")"
		default:
			display = d.Format("Mon 2 Jan 2006")
		}
		m[iso] = &DailySales{Date: iso, Display: display, IsToday: iso == todayISO}
	}
	return ordered, m
}

func hourDisplay(h int) string {
	switch h {
	case 0:
		return "12 AM"
	case 12:
		return "12 PM"
	default:
		if h < 12 {
			return fmt.Sprintf("%d AM", h)
		}
		return fmt.Sprintf("%d PM", h-12)
	}
}

func reportsHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to := parseDateRange(r)
		fromISO := from.Format("2006-01-02")
		toISO := to.Format("2006-01-02")

		ordered, daysMap := buildDayScaffold(from, to)

		// created_at is UTC; +5 hours shifts to Pakistan local
		rows, err := db.Query(`
			SELECT date(created_at, '+5 hours') AS day,
			       SUM(CASE WHEN payment_method = 'cash' THEN total_rupees ELSE 0 END),
			       SUM(CASE WHEN payment_method = 'card' THEN total_rupees ELSE 0 END),
			       SUM(total_rupees),
			       COUNT(*)
			FROM transactions
			WHERE date(created_at, '+5 hours') BETWEEN ? AND ?
			GROUP BY day
		`, fromISO, toISO)
		if err != nil {
			log.Printf("reports query: %v", err)
			http.Error(w, "failed to load report", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var day string
			var cash, card, total, count int
			if err := rows.Scan(&day, &cash, &card, &total, &count); err != nil {
				log.Printf("reports scan: %v", err)
				http.Error(w, "failed to load report", http.StatusInternalServerError)
				return
			}
			if ds, ok := daysMap[day]; ok {
				ds.CashRupees = cash
				ds.CardRupees = card
				ds.TotalRupees = total
				ds.TxnCount = count
			}
		}

		data := ReportsPageData{FromDate: fromISO, ToDate: toISO}
		for _, iso := range ordered {
			ds := *daysMap[iso]
			data.Days = append(data.Days, ds)
			data.GrandCash += ds.CashRupees
			data.GrandCard += ds.CardRupees
			data.GrandTotal += ds.TotalRupees
			data.GrandCount += ds.TxnCount
		}

		hourRows, err := db.Query(`
			SELECT CAST(strftime('%H', created_at, '+5 hours') AS INTEGER) AS hr,
			       SUM(CASE WHEN payment_method = 'cash' THEN total_rupees ELSE 0 END),
			       SUM(CASE WHEN payment_method = 'card' THEN total_rupees ELSE 0 END),
			       SUM(total_rupees),
			       COUNT(*)
			FROM transactions
			WHERE date(created_at, '+5 hours') BETWEEN ? AND ?
			GROUP BY hr
			ORDER BY hr
		`, fromISO, toISO)
		if err != nil {
			log.Printf("reports hour query: %v", err)
			http.Error(w, "failed to load report", http.StatusInternalServerError)
			return
		}
		defer hourRows.Close()
		for hourRows.Next() {
			var hs HourSales
			if err := hourRows.Scan(&hs.Hour, &hs.CashRupees, &hs.CardRupees, &hs.TotalRupees, &hs.TxnCount); err != nil {
				log.Printf("reports hour scan: %v", err)
				http.Error(w, "failed to load report", http.StatusInternalServerError)
				return
			}
			hs.Display = hourDisplay(hs.Hour)
			data.HourRows = append(data.HourRows, hs)
		}

		if err := tmpl.ExecuteTemplate(w, "reports.html", data); err != nil {
			log.Printf("render reports: %v", err)
		}
	}
}

func reportsCSVHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to := parseDateRange(r)
		fromISO := from.Format("2006-01-02")
		toISO := to.Format("2006-01-02")

		ordered, daysMap := buildDayScaffold(from, to)

		rows, err := db.Query(`
			SELECT date(created_at, '+5 hours') AS day,
			       SUM(CASE WHEN payment_method = 'cash' THEN total_rupees ELSE 0 END),
			       SUM(CASE WHEN payment_method = 'card' THEN total_rupees ELSE 0 END),
			       SUM(total_rupees),
			       COUNT(*)
			FROM transactions
			WHERE date(created_at, '+5 hours') BETWEEN ? AND ?
			GROUP BY day
		`, fromISO, toISO)
		if err != nil {
			log.Printf("csv query: %v", err)
			http.Error(w, "failed to export", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var day string
			var cash, card, total, count int
			if err := rows.Scan(&day, &cash, &card, &total, &count); err != nil {
				log.Printf("csv scan: %v", err)
				http.Error(w, "failed to export", http.StatusInternalServerError)
				return
			}
			if ds, ok := daysMap[day]; ok {
				ds.CashRupees = cash
				ds.CardRupees = card
				ds.TotalRupees = total
				ds.TxnCount = count
			}
		}

		filename := fmt.Sprintf("sales-%s-to-%s.csv", fromISO, toISO)
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

		cw := csv.NewWriter(w)
		cw.Write([]string{"Date", "Cash (Rs.)", "Card (Rs.)", "Total (Rs.)", "Transactions"})
		var grandCash, grandCard, grandTotal, grandCount int
		for _, iso := range ordered {
			ds := *daysMap[iso]
			cw.Write([]string{
				ds.Date,
				strconv.Itoa(ds.CashRupees),
				strconv.Itoa(ds.CardRupees),
				strconv.Itoa(ds.TotalRupees),
				strconv.Itoa(ds.TxnCount),
			})
			grandCash += ds.CashRupees
			grandCard += ds.CardRupees
			grandTotal += ds.TotalRupees
			grandCount += ds.TxnCount
		}
		cw.Write([]string{"TOTAL", strconv.Itoa(grandCash), strconv.Itoa(grandCard), strconv.Itoa(grandTotal), strconv.Itoa(grandCount)})
		cw.Flush()
	}
}

func productDrilldownHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		productID, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}

		var productName string
		if err := db.QueryRowContext(r.Context(), "SELECT name FROM products WHERE id = ?", productID).Scan(&productName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			log.Printf("drilldown product query: %v", err)
			http.Error(w, "failed to load product", http.StatusInternalServerError)
			return
		}

		from, to := parseDateRange(r)
		fromISO := from.Format("2006-01-02")
		toISO := to.Format("2006-01-02")

		rows, err := db.QueryContext(r.Context(), `
			SELECT date(t.created_at, '+5 hours') AS day,
			       SUM(ti.quantity) AS qty,
			       SUM(ti.line_total_rupees) AS revenue
			FROM transaction_items ti
			JOIN transactions t ON t.id = ti.transaction_id
			WHERE ti.product_id = ?
			  AND date(t.created_at, '+5 hours') BETWEEN ? AND ?
			GROUP BY day
			ORDER BY day DESC
		`, productID, fromISO, toISO)
		if err != nil {
			log.Printf("drilldown query: %v", err)
			http.Error(w, "failed to load drilldown", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		data := productDrilldownData{
			ProductName: productName,
			FromDate:    fromISO,
			ToDate:      toISO,
		}
		var grandRevenue int
		for rows.Next() {
			var isoDay string
			var qty, revenue int
			if err := rows.Scan(&isoDay, &qty, &revenue); err != nil {
				log.Printf("drilldown scan: %v", err)
				http.Error(w, "failed to load drilldown", http.StatusInternalServerError)
				return
			}
			d, _ := time.ParseInLocation("2006-01-02", isoDay, storeLocation)
			data.Days = append(data.Days, productDaySales{
				Display:   d.Format("Mon 2 Jan 2006"),
				Qty:       qty,
				RevenueRs: formatRupees(revenue),
			})
			data.GrandQty += qty
			grandRevenue += revenue
		}
		data.GrandRevenue = formatRupees(grandRevenue)

		if err := tmpl.ExecuteTemplate(w, "product_drilldown.html", data); err != nil {
			log.Printf("render drilldown: %v", err)
		}
	}
}

type dashboardTopItem struct {
	ProductID int
	Name      string
	Qty       int
	RevenueRs string
}

type dashboardData struct {
	TodaySalesRs  string
	TodayCount    int
	MonthLabel    string
	MonthSalesRs  string
	LowStockCount int
	TopItems      []dashboardTopItem
	ReportDays    []DailySales
	GrandCash     int
	GrandCard     int
	GrandTotal    int
	GrandCount    int
}

func formatRupees(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func dashboardHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		// cashiers get the till, not manager numbers
		if role, _ := r.Context().Value(roleKey).(string); role == "cashier" {
			http.Redirect(w, r, "/pos", http.StatusSeeOther)
			return
		}

		nowLocal := time.Now().In(storeLocation)
		today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, storeLocation)
		todayISO := today.Format("2006-01-02")
		monthKey := nowLocal.Format("2006-01")

		var todayTotal, todayCount int
		err := db.QueryRow(
			`SELECT COALESCE(SUM(total_rupees), 0), COUNT(*)
			 FROM transactions
			 WHERE date(created_at, '+5 hours') = ?`, todayISO,
		).Scan(&todayTotal, &todayCount)
		if err != nil {
			log.Printf("dashboard today query: %v", err)
			http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
			return
		}

		var monthTotal int
		err = db.QueryRow(
			`SELECT COALESCE(SUM(total_rupees), 0)
			 FROM transactions
			 WHERE strftime('%Y-%m', created_at, '+5 hours') = ?`, monthKey,
		).Scan(&monthTotal)
		if err != nil {
			log.Printf("dashboard month query: %v", err)
			http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
			return
		}

		rows, err := db.Query(
			`SELECT p.id, p.name, SUM(ti.quantity) AS qty, SUM(ti.line_total_rupees) AS revenue
			 FROM transaction_items ti
			 JOIN transactions t ON t.id = ti.transaction_id
			 JOIN products p ON p.id = ti.product_id
			 WHERE date(t.created_at, '+5 hours') >= date(?, '-6 days')
			 GROUP BY p.id
			 ORDER BY qty DESC, revenue DESC
			 LIMIT 5`, todayISO,
		)
		if err != nil {
			log.Printf("dashboard top query: %v", err)
			http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var top []dashboardTopItem
		for rows.Next() {
			var it dashboardTopItem
			var revenue int
			if err := rows.Scan(&it.ProductID, &it.Name, &it.Qty, &revenue); err != nil {
				log.Printf("dashboard top scan: %v", err)
				http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
				return
			}
			it.RevenueRs = formatRupees(revenue)
			top = append(top, it)
		}

		var lowStockCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM products WHERE stock < COALESCE(reorder_level, ?)`, lowStockThreshold,
		).Scan(&lowStockCount); err != nil {
			log.Printf("dashboard low stock query: %v", err)
			http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
			return
		}

		daysMap := make(map[string]*DailySales, 7)
		orderedDays := make([]string, 0, 7)
		for i := 0; i < 7; i++ {
			d := today.AddDate(0, 0, -i)
			iso := d.Format("2006-01-02")
			orderedDays = append(orderedDays, iso)
			var display string
			switch i {
			case 0:
				display = "Today (" + d.Format("Mon 2 Jan") + ")"
			case 1:
				display = "Yesterday (" + d.Format("Mon 2 Jan") + ")"
			default:
				display = d.Format("Mon 2 Jan")
			}
			daysMap[iso] = &DailySales{Date: iso, Display: display, IsToday: i == 0}
		}
		reportRows, err := db.Query(`
			SELECT date(created_at, '+5 hours') AS day,
			       SUM(CASE WHEN payment_method = 'cash' THEN total_rupees ELSE 0 END),
			       SUM(CASE WHEN payment_method = 'card' THEN total_rupees ELSE 0 END),
			       SUM(total_rupees),
			       COUNT(*)
			FROM transactions
			WHERE date(created_at, '+5 hours') >= date(?, '-6 days')
			GROUP BY day
		`, todayISO)
		if err != nil {
			log.Printf("dashboard report query: %v", err)
			http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
			return
		}
		defer reportRows.Close()
		for reportRows.Next() {
			var day string
			var cash, card, total, count int
			if err := reportRows.Scan(&day, &cash, &card, &total, &count); err != nil {
				log.Printf("dashboard report scan: %v", err)
				http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
				return
			}
			if ds, ok := daysMap[day]; ok {
				ds.CashRupees = cash
				ds.CardRupees = card
				ds.TotalRupees = total
				ds.TxnCount = count
			}
		}
		reportDays := make([]DailySales, 0, 7)
		data := dashboardData{
			TodaySalesRs:  formatRupees(todayTotal),
			TodayCount:    todayCount,
			MonthLabel:    nowLocal.Format("Jan 2006"),
			MonthSalesRs:  formatRupees(monthTotal),
			LowStockCount: lowStockCount,
			TopItems:      top,
		}
		for _, iso := range orderedDays {
			ds := *daysMap[iso]
			reportDays = append(reportDays, ds)
			data.GrandCash += ds.CashRupees
			data.GrandCard += ds.CardRupees
			data.GrandTotal += ds.TotalRupees
			data.GrandCount += ds.TxnCount
		}
		data.ReportDays = reportDays

		if err := tmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
			log.Printf("render dashboard: %v", err)
		}
	}
}

func stockUpdateHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ProductID int `json:"product_id"`
			Stock     int `json:"stock"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad request")
			return
		}
		if req.Stock < 0 {
			writeJSONError(w, http.StatusBadRequest, "stock cannot be negative")
			return
		}

		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			log.Printf("begin stock update tx: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "update failed")
			return
		}
		defer tx.Rollback()

		var oldStock int
		err = tx.QueryRowContext(ctx, "SELECT stock FROM products WHERE id = ?", req.ProductID).Scan(&oldStock)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("product %d not found", req.ProductID))
			return
		}
		if err != nil {
			log.Printf("stock read: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "update failed")
			return
		}

		if req.Stock != oldStock {
			if _, err := tx.ExecContext(ctx, "UPDATE products SET stock = ? WHERE id = ?", req.Stock, req.ProductID); err != nil {
				log.Printf("stock update: %v", err)
				writeJSONError(w, http.StatusInternalServerError, "update failed")
				return
			}
			if err := recordMovement(ctx, tx, req.ProductID, req.Stock-oldStock, req.Stock, "manual_adjust"); err != nil {
				log.Printf("record adjust movement: %v", err)
				writeJSONError(w, http.StatusInternalServerError, "update failed")
				return
			}
		}

		if err := tx.Commit(); err != nil {
			log.Printf("commit stock update: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "update failed")
			return
		}

		additions := recentAdditions(db, req.ProductID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"product_id":       req.ProductID,
			"stock":            req.Stock,
			"recent_additions": additions,
		})
	}
}

type Supplier struct {
	ID            int
	Name          string
	Phone         string
	ContactPerson string
	Address       string
	Active        bool
}

type supplierListData struct {
	Suppliers []Supplier
}

type supplierFormData struct {
	Supplier Supplier
	IsEdit   bool
	Error    string
}

type ReceivingSession struct {
	ID           int
	Label        string
	SupplierName string
	StartedAt    string
	ItemCount    int
	TotalQty     int
}

type receivingListData struct {
	Sessions  []ReceivingSession
	Suppliers []Supplier
	Error     string
}

func loadActiveSuppliers(db *sql.DB) ([]Supplier, error) {
	rows, err := db.Query(`SELECT id, name FROM suppliers WHERE active = 1 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Supplier
	for rows.Next() {
		var s Supplier
		if err := rows.Scan(&s.ID, &s.Name); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func renderReceivingList(db *sql.DB, tmpl *template.Template, w http.ResponseWriter, errMsg string) {
	rows, err := db.Query(
		`SELECT s.id, s.label, COALESCE(sup.name,''), s.started_at,
		        COUNT(i.id) AS item_count,
		        COALESCE(SUM(i.qty), 0) AS total_qty
		 FROM receiving_sessions s
		 LEFT JOIN receiving_session_items i ON i.session_id = s.id
		 LEFT JOIN suppliers sup ON sup.id = s.supplier_id
		 WHERE s.status = 'active'
		 GROUP BY s.id
		 ORDER BY s.started_at DESC`,
	)
	if err != nil {
		log.Printf("receiving list query: %v", err)
		http.Error(w, "failed to load sessions", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	data := receivingListData{Error: errMsg}
	for rows.Next() {
		var s ReceivingSession
		var startedAt time.Time
		if err := rows.Scan(&s.ID, &s.Label, &s.SupplierName, &startedAt, &s.ItemCount, &s.TotalQty); err != nil {
			log.Printf("receiving list scan: %v", err)
			http.Error(w, "failed to load sessions", http.StatusInternalServerError)
			return
		}
		s.StartedAt = startedAt.In(storeLocation).Format("2 Jan, 3:04 PM")
		data.Sessions = append(data.Sessions, s)
	}

	suppliers, err := loadActiveSuppliers(db)
	if err != nil {
		log.Printf("receiving list suppliers: %v", err)
		http.Error(w, "failed to load suppliers", http.StatusInternalServerError)
		return
	}
	data.Suppliers = suppliers

	if err := tmpl.ExecuteTemplate(w, "receiving.html", data); err != nil {
		log.Printf("render receiving: %v", err)
	}
}

func receivingListHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderReceivingList(db, tmpl, w, "")
	}
}

func createReceivingSessionHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		label := strings.TrimSpace(r.FormValue("label"))
		if label == "" {
			renderReceivingList(db, tmpl, w, "Give the shipment a label before starting.")
			return
		}

		var supplierID sql.NullInt64
		if sid := strings.TrimSpace(r.FormValue("supplier_id")); sid != "" {
			if n, err := strconv.ParseInt(sid, 10, 64); err == nil {
				supplierID = sql.NullInt64{Int64: n, Valid: true}
			}
		}

		res, err := db.Exec("INSERT INTO receiving_sessions (label, supplier_id) VALUES (?, ?)", label, supplierID)
		if err != nil {
			log.Printf("create receiving session: %v", err)
			renderReceivingList(db, tmpl, w, "Failed to start session.")
			return
		}
		id, err := res.LastInsertId()
		if err != nil {
			log.Printf("receiving session last id: %v", err)
			renderReceivingList(db, tmpl, w, "Failed to start session.")
			return
		}

		http.Redirect(w, r, fmt.Sprintf("/receiving/%d", id), http.StatusSeeOther)
	}
}

type ReceivingItem struct {
	ProductID int    `json:"product_id"`
	Barcode   string `json:"barcode"`
	Name      string `json:"name"`
	Qty       int    `json:"qty"`
	Stock     int    `json:"stock"`
}

type receivingDetailData struct {
	ID           int
	Label        string
	SupplierName string
	StartedAt    string
	FinalizedAt  string
	Status       string
	TotalQty     int
	Error        string
	ItemsJSON    template.JS
}

func loadReceivingItems(db *sql.DB, sessionID int) ([]ReceivingItem, int, error) {
	rows, err := db.Query(
		`SELECT p.id, p.barcode, p.name, i.qty, p.stock
		 FROM receiving_session_items i
		 JOIN products p ON p.id = i.product_id
		 WHERE i.session_id = ?
		 ORDER BY p.name`, sessionID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := []ReceivingItem{}
	total := 0
	for rows.Next() {
		var it ReceivingItem
		if err := rows.Scan(&it.ProductID, &it.Barcode, &it.Name, &it.Qty, &it.Stock); err != nil {
			return nil, 0, err
		}
		total += it.Qty
		items = append(items, it)
	}
	return items, total, rows.Err()
}

func receivingSessionStatus(db *sql.DB, id int) (status string, found bool, err error) {
	err = db.QueryRow("SELECT status FROM receiving_sessions WHERE id = ?", id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return status, true, nil
}

func receivingDetailHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}

		var label, status, supplierName string
		var startedAt time.Time
		var finalizedAt sql.NullTime
		err = db.QueryRow(
			`SELECT rs.label, rs.status, rs.started_at, rs.finalized_at, COALESCE(sup.name,'')
			 FROM receiving_sessions rs
			 LEFT JOIN suppliers sup ON sup.id = rs.supplier_id
			 WHERE rs.id = ?`, id,
		).Scan(&label, &status, &startedAt, &finalizedAt, &supplierName)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			log.Printf("receiving detail query: %v", err)
			http.Error(w, "failed to load session", http.StatusInternalServerError)
			return
		}

		items, total, err := loadReceivingItems(db, id)
		if err != nil {
			log.Printf("receiving detail items: %v", err)
			http.Error(w, "failed to load session", http.StatusInternalServerError)
			return
		}
		itemsBytes, err := json.Marshal(items)
		if err != nil {
			log.Printf("receiving detail marshal: %v", err)
			http.Error(w, "failed to load session", http.StatusInternalServerError)
			return
		}

		data := receivingDetailData{
			ID:           id,
			Label:        label,
			SupplierName: supplierName,
			Status:       status,
			StartedAt:    startedAt.In(storeLocation).Format("2 Jan, 3:04 PM"),
			TotalQty:     total,
			ItemsJSON:    template.JS(itemsBytes),
		}
		if finalizedAt.Valid {
			data.FinalizedAt = finalizedAt.Time.In(storeLocation).Format("2 Jan, 3:04 PM")
		}
		if r.URL.Query().Get("err") == "empty" {
			data.Error = "Scan at least one item before finalizing."
		}
		if err := tmpl.ExecuteTemplate(w, "receiving_detail.html", data); err != nil {
			log.Printf("render receiving_detail: %v", err)
		}
	}
}

func writeReceivingItems(db *sql.DB, w http.ResponseWriter, sessionID int, scanned string) {
	items, total, err := loadReceivingItems(db, sessionID)
	if err != nil {
		log.Printf("reload receiving items: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to load items")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"items":      items,
		"total_qty":  total,
		"item_count": len(items),
		"scanned":    scanned,
	})
}

func receivingScanHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad session id")
			return
		}
		status, found, err := receivingSessionStatus(db, id)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "session not found")
			return
		}
		if status != "active" {
			writeJSONError(w, http.StatusConflict, "session already finalized")
			return
		}

		var req struct {
			Barcode string `json:"barcode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad request")
			return
		}
		barcode := normalizeBarcode(req.Barcode)
		if barcode == "" {
			writeJSONError(w, http.StatusBadRequest, "barcode required")
			return
		}
		alt := altBarcode(barcode)
		if alt == "" {
			alt = barcode
		}

		var productID int
		var name string
		err = db.QueryRow("SELECT id, name FROM products WHERE barcode = ? OR barcode = ?", barcode, alt).Scan(&productID, &name)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "barcode not found")
			return
		}
		if err != nil {
			log.Printf("receiving scan lookup: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "lookup failed")
			return
		}

		_, err = db.Exec(
			`INSERT INTO receiving_session_items (session_id, product_id, qty)
			 VALUES (?, ?, 1)
			 ON CONFLICT(session_id, product_id) DO UPDATE SET qty = qty + 1`,
			id, productID,
		)
		if err != nil {
			log.Printf("receiving scan upsert: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to add item")
			return
		}

		writeReceivingItems(db, w, id, name)
	}
}

func receivingItemHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad session id")
			return
		}
		status, found, err := receivingSessionStatus(db, id)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "session not found")
			return
		}
		if status != "active" {
			writeJSONError(w, http.StatusConflict, "session already finalized")
			return
		}

		var req struct {
			ProductID int    `json:"product_id"`
			Op        string `json:"op"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad request")
			return
		}

		switch req.Op {
		case "inc":
			_, err = db.Exec(
				"UPDATE receiving_session_items SET qty = qty + 1 WHERE session_id = ? AND product_id = ?",
				id, req.ProductID)
		case "dec":

			ctx := r.Context()
			var tx *sql.Tx
			tx, err = db.BeginTx(ctx, nil)
			if err == nil {
				_, err = tx.ExecContext(ctx,
					"UPDATE receiving_session_items SET qty = qty - 1 WHERE session_id = ? AND product_id = ?",
					id, req.ProductID)
				if err == nil {
					_, err = tx.ExecContext(ctx,
						"DELETE FROM receiving_session_items WHERE session_id = ? AND product_id = ? AND qty <= 0",
						id, req.ProductID)
				}
				if err == nil {
					err = tx.Commit()
				} else {
					tx.Rollback()
				}
			}
		case "remove":
			_, err = db.Exec(
				"DELETE FROM receiving_session_items WHERE session_id = ? AND product_id = ?",
				id, req.ProductID)
		default:
			writeJSONError(w, http.StatusBadRequest, "unknown op")
			return
		}
		if err != nil {
			log.Printf("receiving item %q: %v", req.Op, err)
			writeJSONError(w, http.StatusInternalServerError, "failed to update item")
			return
		}

		writeReceivingItems(db, w, id, "")
	}
}

func receivingDeleteHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}

		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			log.Printf("begin delete session tx: %v", err)
			http.Error(w, "failed to delete session", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var status string
		err = tx.QueryRowContext(ctx, "SELECT status FROM receiving_sessions WHERE id = ?", id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && status != "active") {
			http.Redirect(w, r, "/receiving", http.StatusSeeOther)
			return
		}
		if err != nil {
			log.Printf("delete session status: %v", err)
			http.Error(w, "failed to delete session", http.StatusInternalServerError)
			return
		}

		// items reference the session (FK), so they go first
		if _, err := tx.ExecContext(ctx, "DELETE FROM receiving_session_items WHERE session_id = ?", id); err != nil {
			log.Printf("delete session items: %v", err)
			http.Error(w, "failed to delete session", http.StatusInternalServerError)
			return
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM receiving_sessions WHERE id = ?", id); err != nil {
			log.Printf("delete session: %v", err)
			http.Error(w, "failed to delete session", http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			log.Printf("commit delete session: %v", err)
			http.Error(w, "failed to delete session", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/receiving", http.StatusSeeOther)
	}
}

func receivingFinalizeHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}

		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			log.Printf("begin finalize tx: %v", err)
			http.Error(w, "failed to finalize", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var status string
		err = tx.QueryRowContext(ctx, "SELECT status FROM receiving_sessions WHERE id = ?", id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			log.Printf("finalize status: %v", err)
			http.Error(w, "failed to finalize", http.StatusInternalServerError)
			return
		}
		if status != "active" {
			http.Redirect(w, r, fmt.Sprintf("/receiving/%d", id), http.StatusSeeOther)
			return
		}

		type line struct {
			productID int
			qty       int
		}
		rows, err := tx.QueryContext(ctx,
			"SELECT product_id, qty FROM receiving_session_items WHERE session_id = ?", id)
		if err != nil {
			log.Printf("finalize load items: %v", err)
			http.Error(w, "failed to finalize", http.StatusInternalServerError)
			return
		}
		var lines []line
		for rows.Next() {
			var l line
			if err := rows.Scan(&l.productID, &l.qty); err != nil {
				rows.Close()
				log.Printf("finalize scan: %v", err)
				http.Error(w, "failed to finalize", http.StatusInternalServerError)
				return
			}
			lines = append(lines, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			log.Printf("finalize rows: %v", err)
			http.Error(w, "failed to finalize", http.StatusInternalServerError)
			return
		}

		if len(lines) == 0 {
			http.Redirect(w, r, fmt.Sprintf("/receiving/%d?err=empty", id), http.StatusSeeOther)
			return
		}

		for _, l := range lines {
			var newStock int
			// RETURNING gives the post-update stock for the audit row
			err := tx.QueryRowContext(ctx,
				"UPDATE products SET stock = stock + ? WHERE id = ? RETURNING stock",
				l.qty, l.productID,
			).Scan(&newStock)
			if err != nil {
				log.Printf("finalize update stock: %v", err)
				http.Error(w, "failed to finalize", http.StatusInternalServerError)
				return
			}
			if err := recordMovement(ctx, tx, l.productID, l.qty, newStock, "receiving"); err != nil {
				log.Printf("finalize record movement: %v", err)
				http.Error(w, "failed to finalize", http.StatusInternalServerError)
				return
			}
		}

		if _, err := tx.ExecContext(ctx,
			"UPDATE receiving_sessions SET status = 'finalized', finalized_at = CURRENT_TIMESTAMP WHERE id = ?",
			id,
		); err != nil {
			log.Printf("finalize session: %v", err)
			http.Error(w, "failed to finalize", http.StatusInternalServerError)
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("commit finalize: %v", err)
			http.Error(w, "failed to finalize", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, fmt.Sprintf("/receiving/%d", id), http.StatusSeeOther)
	}
}

func suppliersHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(),
			`SELECT id, name, COALESCE(phone,''), COALESCE(contact_person,''), COALESCE(address,''), active
			 FROM suppliers ORDER BY name`)
		if err != nil {
			log.Printf("suppliers query: %v", err)
			http.Error(w, "failed to load suppliers", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var data supplierListData
		for rows.Next() {
			var s Supplier
			var active int
			if err := rows.Scan(&s.ID, &s.Name, &s.Phone, &s.ContactPerson, &s.Address, &active); err != nil {
				log.Printf("suppliers scan: %v", err)
				http.Error(w, "failed to load suppliers", http.StatusInternalServerError)
				return
			}
			s.Active = active == 1
			data.Suppliers = append(data.Suppliers, s)
		}
		if err := tmpl.ExecuteTemplate(w, "suppliers.html", data); err != nil {
			log.Printf("render suppliers: %v", err)
		}
	}
}

func newSupplierFormHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := tmpl.ExecuteTemplate(w, "supplier_form.html", supplierFormData{}); err != nil {
			log.Printf("render supplier form: %v", err)
		}
	}
}

func createSupplierHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			tmpl.ExecuteTemplate(w, "supplier_form.html", supplierFormData{
				Error: "Supplier name is required.",
			})
			return
		}
		_, err := db.ExecContext(r.Context(),
			`INSERT INTO suppliers (name, phone, contact_person, address) VALUES (?, ?, ?, ?)`,
			name,
			strings.TrimSpace(r.FormValue("phone")),
			strings.TrimSpace(r.FormValue("contact_person")),
			strings.TrimSpace(r.FormValue("address")),
		)
		if err != nil {
			log.Printf("create supplier: %v", err)
			tmpl.ExecuteTemplate(w, "supplier_form.html", supplierFormData{
				Supplier: Supplier{
					Name:          name,
					Phone:         strings.TrimSpace(r.FormValue("phone")),
					ContactPerson: strings.TrimSpace(r.FormValue("contact_person")),
					Address:       strings.TrimSpace(r.FormValue("address")),
				},
				Error: "Failed to save supplier.",
			})
			return
		}
		http.Redirect(w, r, "/suppliers", http.StatusSeeOther)
	}
}

func editSupplierFormHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var s Supplier
		var active int
		err = db.QueryRowContext(r.Context(),
			`SELECT id, name, COALESCE(phone,''), COALESCE(contact_person,''), COALESCE(address,''), active
			 FROM suppliers WHERE id = ?`, id,
		).Scan(&s.ID, &s.Name, &s.Phone, &s.ContactPerson, &s.Address, &active)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			log.Printf("edit supplier query: %v", err)
			http.Error(w, "failed to load supplier", http.StatusInternalServerError)
			return
		}
		s.Active = active == 1
		if err := tmpl.ExecuteTemplate(w, "supplier_form.html", supplierFormData{
			Supplier: s, IsEdit: true,
		}); err != nil {
			log.Printf("render supplier form: %v", err)
		}
	}
}

func updateSupplierHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(r.FormValue("name"))
		active := 0
		if r.FormValue("active") == "1" {
			active = 1
		}
		if name == "" {
			tmpl.ExecuteTemplate(w, "supplier_form.html", supplierFormData{
				Supplier: Supplier{
					ID:            id,
					Name:          name,
					Phone:         strings.TrimSpace(r.FormValue("phone")),
					ContactPerson: strings.TrimSpace(r.FormValue("contact_person")),
					Address:       strings.TrimSpace(r.FormValue("address")),
					Active:        active == 1,
				},
				IsEdit: true,
				Error:  "Supplier name is required.",
			})
			return
		}
		_, err = db.ExecContext(r.Context(),
			`UPDATE suppliers SET name=?, phone=?, contact_person=?, address=?, active=? WHERE id=?`,
			name,
			strings.TrimSpace(r.FormValue("phone")),
			strings.TrimSpace(r.FormValue("contact_person")),
			strings.TrimSpace(r.FormValue("address")),
			active, id,
		)
		if err != nil {
			log.Printf("update supplier: %v", err)
			tmpl.ExecuteTemplate(w, "supplier_form.html", supplierFormData{
				Supplier: Supplier{
					ID:            id,
					Name:          name,
					Phone:         strings.TrimSpace(r.FormValue("phone")),
					ContactPerson: strings.TrimSpace(r.FormValue("contact_person")),
					Address:       strings.TrimSpace(r.FormValue("address")),
					Active:        active == 1,
				},
				IsEdit: true,
				Error:  "Failed to save changes.",
			})
			return
		}
		http.Redirect(w, r, "/suppliers", http.StatusSeeOther)
	}
}

func normalizeBarcode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func validateBarcodeLength(s string) error {
	n := len(s)
	if n != 8 && n != 12 && n != 13 && n != 14 {
		return fmt.Errorf("barcode must be 8, 12, 13, or 14 digits (got %d)", n)
	}
	return nil
}

// validateBarcodeCheckDigit verifies the GS1 mod-10 checksum (last digit).
// Works for EAN-8, UPC-A (12), EAN-13, ITF-14: the rightmost data digit always
// gets weight 3, alternating 3,1,3,1 leftward. Call after validateBarcodeLength;
// assumes digits-only input (normalizeBarcode guarantees this).
func validateBarcodeCheckDigit(s string) error {
	n := len(s)
	sum := 0
	for i := 0; i < n-1; i++ {
		d := int(s[n-2-i] - '0')
		if i%2 == 0 {
			sum += d * 3
		} else {
			sum += d
		}
	}
	expected := (10 - sum%10) % 10
	if expected != int(s[n-1]-'0') {
		return fmt.Errorf("barcode check digit is wrong (expected last digit %d)", expected)
	}
	return nil
}

// altBarcode returns the equivalent UPC-A/EAN-13 form of a barcode, or "".
// A 12-digit UPC-A equals the 13-digit EAN-13 with a leading zero, and vice versa.
// Used on the read side so a scanned code matches whichever form is stored.
func altBarcode(code string) string {
	if len(code) == 12 {
		return "0" + code
	}
	if len(code) == 13 && code[0] == '0' {
		return code[1:]
	}
	return ""
}

func recordMovement(ctx context.Context, tx *sql.Tx, productID, delta, newStock int, reason string) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO stock_movements (product_id, delta, new_stock, reason) VALUES (?, ?, ?, ?)",
		productID, delta, newStock, reason,
	)
	return err
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type ProductFormData struct {
	Barcode     string
	Name        string
	PriceRupees string
	Stock       string
	Category    string
	Return      string
	Error       string
}

// open-redirect guard: local single-"/" paths only
func validateReturnPath(s string) string {
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return ""
	}
	return s
}

func newProductFormHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		rawReturn := r.URL.Query().Get("return")
		validReturn := validateReturnPath(rawReturn)
		if rawReturn != "" && validReturn == "" {
			q := r.URL.Query()
			q.Del("return")
			cleanURL := r.URL.Path
			if encoded := q.Encode(); encoded != "" {
				cleanURL += "?" + encoded
			}
			http.Redirect(w, r, cleanURL, http.StatusSeeOther)
			return
		}

		data := ProductFormData{
			Barcode: normalizeBarcode(r.URL.Query().Get("barcode")),
			Return:  validReturn,
		}
		if err := tmpl.ExecuteTemplate(w, "new_product.html", data); err != nil {
			log.Printf("render new_product: %v", err)
		}
	}
}

func createProductHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}

		form := ProductFormData{
			Barcode:     strings.TrimSpace(r.FormValue("barcode")),
			Name:        strings.TrimSpace(r.FormValue("name")),
			PriceRupees: strings.TrimSpace(r.FormValue("price_rupees")),
			Stock:       strings.TrimSpace(r.FormValue("stock")),
			Category:    strings.TrimSpace(r.FormValue("category")),
			Return:      validateReturnPath(r.FormValue("return")),
		}

		fail := func(msg string) {
			form.Error = msg
			if err := tmpl.ExecuteTemplate(w, "new_product.html", form); err != nil {
				log.Printf("render new_product: %v", err)
			}
		}

		if form.Barcode == "" {
			fail("Barcode is required")
			return
		}

		normalized := normalizeBarcode(form.Barcode)
		if normalized == "" {
			fail("Barcode must contain digits")
			return
		}
		if err := validateBarcodeLength(normalized); err != nil {
			fail(strings.ToUpper(err.Error()[:1]) + err.Error()[1:])
			return
		}
		if err := validateBarcodeCheckDigit(normalized); err != nil {
			fail(strings.ToUpper(err.Error()[:1]) + err.Error()[1:])
			return
		}
		form.Barcode = normalized

		// Reject the UPC-A/EAN-13 twin of an existing product. The read side matches
		// either form, so storing both would make one physical item resolve to two rows.
		if alt := altBarcode(normalized); alt != "" {
			var one int
			err := db.QueryRow("SELECT 1 FROM products WHERE barcode = ?", alt).Scan(&one)
			if err == nil {
				fail("A product with the equivalent UPC-A/EAN-13 barcode already exists")
				return
			}
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("twin barcode check: %v", err)
				fail("Failed to add product")
				return
			}
		}
		if form.Name == "" {
			fail("Name is required")
			return
		}
		price, err := strconv.Atoi(form.PriceRupees)
		if err != nil || price < 0 {
			fail("Price must be a non-negative whole number")
			return
		}
		stock, err := strconv.Atoi(form.Stock)
		if err != nil || stock < 0 {
			fail("Stock must be a non-negative whole number")
			return
		}

		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			log.Printf("begin create product tx: %v", err)
			fail("Failed to add product")
			return
		}
		defer tx.Rollback()

		cat := interface{}(nil)
		if form.Category != "" {
			cat = form.Category
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO products (barcode, name, price_rupees, stock, category) VALUES (?, ?, ?, ?, ?)",
			form.Barcode, form.Name, price, stock, cat,
		)
		if err != nil {

			// SQLite reports a duplicate barcode as a UNIQUE constraint error
			if strings.Contains(err.Error(), "UNIQUE") {
				fail("A product with that barcode already exists")
				return
			}
			log.Printf("insert product: %v", err)
			fail("Failed to add product")
			return
		}

		if stock > 0 {
			productID, err := res.LastInsertId()
			if err != nil {
				log.Printf("last insert id: %v", err)
				fail("Failed to add product")
				return
			}
			if err := recordMovement(ctx, tx, int(productID), stock, stock, "initial"); err != nil {
				log.Printf("record initial movement: %v", err)
				fail("Failed to add product")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("commit create product: %v", err)
			fail("Failed to add product")
			return
		}

		target := form.Return
		if target == "" {
			target = "/products"
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

type productsPageData struct {
	Products       []Product
	LowCount       int
	LowOnly        bool
	CategoryFilter string
	Threshold      int
}

func productsHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lowOnly := r.URL.Query().Get("low") == "1"
		category := strings.TrimSpace(r.URL.Query().Get("category"))

		query := `SELECT p.id, p.barcode, p.name, p.price_rupees, p.stock, COALESCE(p.category,''),
		       COALESCE(p.reorder_level, ?), p.reorder_qty, COALESCE(s.name, '')
		FROM products p
		LEFT JOIN suppliers s ON s.id = p.supplier_id`
		args := []any{lowStockThreshold}

		var wheres []string
		if lowOnly {
			wheres = append(wheres, "p.stock < COALESCE(p.reorder_level, ?)")
			args = append(args, lowStockThreshold)
		}
		if category != "" {
			wheres = append(wheres, "p.category = ?")
			args = append(args, category)
		}
		if len(wheres) > 0 {
			query += " WHERE " + strings.Join(wheres, " AND ")
		}
		query += " ORDER BY p.name"

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var products []Product
		for rows.Next() {
			var p Product
			if err := rows.Scan(&p.ID, &p.Barcode, &p.Name, &p.PriceRupees, &p.Stock, &p.Category, &p.Threshold, &p.ReorderQty, &p.SupplierName); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			products = append(products, p)
		}
		if err := rows.Err(); err != nil {
			log.Printf("products rows: %v", err)
			http.Error(w, "failed to load products", http.StatusInternalServerError)
			return
		}

		var lowCount int
		if err := db.QueryRow("SELECT COUNT(*) FROM products WHERE stock < COALESCE(reorder_level, ?)", lowStockThreshold).Scan(&lowCount); err != nil {
			log.Printf("count low stock: %v", err)

		}

		data := productsPageData{
			Products:       products,
			LowCount:       lowCount,
			LowOnly:        lowOnly,
			CategoryFilter: category,
			Threshold:      lowStockThreshold,
		}
		if err := tmpl.ExecuteTemplate(w, "products.html", data); err != nil {
			log.Printf("render products: %v", err)
		}
	}
}

func reportsCategoriesHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to := parseDateRange(r)
		fromISO := from.Format("2006-01-02")
		toISO := to.Format("2006-01-02")

		rows, err := db.Query(`
			SELECT COALESCE(p.category, 'Uncategorized'),
			       SUM(ti.quantity),
			       SUM(ti.line_total_rupees),
			       COALESCE(SUM(ti.quantity * p.cost_price_rupees), 0)
			FROM transaction_items ti
			JOIN products p ON p.id = ti.product_id
			JOIN transactions t ON t.id = ti.transaction_id
			WHERE date(t.created_at, '+5 hours') BETWEEN ? AND ?
			GROUP BY p.category
			ORDER BY SUM(ti.line_total_rupees) DESC
		`, fromISO, toISO)
		if err != nil {
			log.Printf("categories report query: %v", err)
			http.Error(w, "failed to load report", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		data := reportsCategoriesPageData{FromDate: fromISO, ToDate: toISO}
		for rows.Next() {
			var cs CategorySales
			var cost sql.NullInt64
			if err := rows.Scan(&cs.Category, &cs.Qty, &cs.RevenueRs, &cost); err != nil {
				log.Printf("categories report scan: %v", err)
				http.Error(w, "failed to load report", http.StatusInternalServerError)
				return
			}
			if cost.Valid {
				cs.CostRs = int(cost.Int64)
			}
			cs.ProfitRs = cs.RevenueRs - cs.CostRs
			data.Rows = append(data.Rows, cs)
			data.GrandQty += cs.Qty
			data.GrandRev += cs.RevenueRs
			data.GrandCost += cs.CostRs
			data.GrandProfit += cs.ProfitRs
		}
		if err := rows.Err(); err != nil {
			log.Printf("categories rows: %v", err)
			http.Error(w, "failed to load report", http.StatusInternalServerError)
			return
		}

		if err := tmpl.ExecuteTemplate(w, "reports_categories.html", data); err != nil {
			log.Printf("render reports_categories: %v", err)
		}
	}
}

type editProductData struct {
	Product      Product
	Suppliers    []Supplier
	Barcode      string
	Name         string
	PriceRupees  string
	Stock        string
	Category     string
	SupplierID   string
	ReorderLevel string
	ReorderQty   string
	Error        string
}

// nullIntStr renders a nullable INTEGER column as a form value ("" = NULL).
func nullIntStr(n sql.NullInt64) string {
	if !n.Valid {
		return ""
	}
	return strconv.FormatInt(n.Int64, 10)
}

func queryActiveSuppliers(ctx context.Context, db *sql.DB) ([]Supplier, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM suppliers WHERE active = 1 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ss []Supplier
	for rows.Next() {
		var s Supplier
		if err := rows.Scan(&s.ID, &s.Name); err != nil {
			return nil, err
		}
		ss = append(ss, s)
	}
	return ss, rows.Err()
}

func editProductFormHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var p Product
		err = db.QueryRowContext(r.Context(),
			"SELECT id, barcode, name, price_rupees, stock, COALESCE(category,''), supplier_id, reorder_level, reorder_qty FROM products WHERE id = ?",
			id,
		).Scan(&p.ID, &p.Barcode, &p.Name, &p.PriceRupees, &p.Stock, &p.Category, &p.SupplierID, &p.ReorderLevel, &p.ReorderQty)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			log.Printf("edit product query: %v", err)
			http.Error(w, "failed to load product", http.StatusInternalServerError)
			return
		}

		suppliers, err := queryActiveSuppliers(r.Context(), db)
		if err != nil {
			log.Printf("edit product suppliers: %v", err)
			http.Error(w, "failed to load suppliers", http.StatusInternalServerError)
			return
		}

		data := editProductData{
			Product:      p,
			Suppliers:    suppliers,
			Barcode:      p.Barcode,
			Name:         p.Name,
			PriceRupees:  strconv.Itoa(p.PriceRupees),
			Stock:        strconv.Itoa(p.Stock),
			Category:     p.Category,
			SupplierID:   nullIntStr(p.SupplierID),
			ReorderLevel: nullIntStr(p.ReorderLevel),
			ReorderQty:   nullIntStr(p.ReorderQty),
		}
		if err := tmpl.ExecuteTemplate(w, "edit_product.html", data); err != nil {
			log.Printf("render edit_product: %v", err)
		}
	}
}

func editProductHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(r.FormValue("name"))
		priceStr := strings.TrimSpace(r.FormValue("price_rupees"))
		stockStr := strings.TrimSpace(r.FormValue("stock"))
		category := strings.TrimSpace(r.FormValue("category"))
		supplierStr := strings.TrimSpace(r.FormValue("supplier_id"))
		reorderLevelStr := strings.TrimSpace(r.FormValue("reorder_level"))
		reorderQtyStr := strings.TrimSpace(r.FormValue("reorder_qty"))

		if name == "" {
			renderEditProductError(db, tmpl, w, r, id, "Name is required")
			return
		}
		price, err := strconv.Atoi(priceStr)
		if err != nil || price < 0 {
			renderEditProductError(db, tmpl, w, r, id, "Price must be a non-negative whole number")
			return
		}
		stock, err := strconv.Atoi(stockStr)
		if err != nil || stock < 0 {
			renderEditProductError(db, tmpl, w, r, id, "Stock must be a non-negative whole number")
			return
		}

		var supID interface{}
		if supplierStr != "" {
			sid, err := strconv.Atoi(supplierStr)
			if err != nil {
				renderEditProductError(db, tmpl, w, r, id, "Invalid supplier")
				return
			}
			supID = sid
		}

		// blank = NULL = use the global default threshold
		var reorderLevel interface{}
		if reorderLevelStr != "" {
			n, err := strconv.Atoi(reorderLevelStr)
			if err != nil || n < 0 {
				renderEditProductError(db, tmpl, w, r, id, "Reorder level must be a non-negative whole number (or blank for default)")
				return
			}
			reorderLevel = n
		}
		var reorderQty interface{}
		if reorderQtyStr != "" {
			n, err := strconv.Atoi(reorderQtyStr)
			if err != nil || n < 0 {
				renderEditProductError(db, tmpl, w, r, id, "Reorder qty must be a non-negative whole number (or blank)")
				return
			}
			reorderQty = n
		}

		// stock can change here, so this write follows the same rule as every
		// other stock writer: update + recordMovement in one transaction
		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			log.Printf("begin edit product tx: %v", err)
			renderEditProductError(db, tmpl, w, r, id, "Failed to save changes")
			return
		}
		defer tx.Rollback()

		var oldStock int
		err = tx.QueryRowContext(ctx, "SELECT stock FROM products WHERE id = ?", id).Scan(&oldStock)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			log.Printf("edit product stock read: %v", err)
			renderEditProductError(db, tmpl, w, r, id, "Failed to save changes")
			return
		}

		if _, err := tx.ExecContext(ctx,
			"UPDATE products SET name=?, price_rupees=?, stock=?, category=?, supplier_id=?, reorder_level=?, reorder_qty=? WHERE id=?",
			name, price, stock, category, supID, reorderLevel, reorderQty, id,
		); err != nil {
			log.Printf("update product: %v", err)
			renderEditProductError(db, tmpl, w, r, id, "Failed to save changes")
			return
		}
		if stock != oldStock {
			if err := recordMovement(ctx, tx, id, stock-oldStock, stock, "manual_adjust"); err != nil {
				log.Printf("record edit movement: %v", err)
				renderEditProductError(db, tmpl, w, r, id, "Failed to save changes")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("commit edit product: %v", err)
			renderEditProductError(db, tmpl, w, r, id, "Failed to save changes")
			return
		}
		http.Redirect(w, r, "/products", http.StatusSeeOther)
	}
}

func renderEditProductError(db *sql.DB, tmpl *template.Template, w http.ResponseWriter, r *http.Request, id int, msg string) {
	var p Product
	err := db.QueryRowContext(r.Context(),
		"SELECT id, barcode, name, price_rupees, stock, COALESCE(category,''), supplier_id, reorder_level, reorder_qty FROM products WHERE id = ?",
		id,
	).Scan(&p.ID, &p.Barcode, &p.Name, &p.PriceRupees, &p.Stock, &p.Category, &p.SupplierID, &p.ReorderLevel, &p.ReorderQty)
	if err != nil {
		http.Error(w, "product not found", http.StatusInternalServerError)
		return
	}
	suppliers, _ := queryActiveSuppliers(r.Context(), db)
	data := editProductData{
		Product:      p,
		Suppliers:    suppliers,
		Barcode:      p.Barcode,
		Name:         p.Name,
		PriceRupees:  strconv.Itoa(p.PriceRupees),
		Stock:        strconv.Itoa(p.Stock),
		Category:     p.Category,
		SupplierID:   nullIntStr(p.SupplierID),
		ReorderLevel: nullIntStr(p.ReorderLevel),
		ReorderQty:   nullIntStr(p.ReorderQty),
		Error:        msg,
	}
	tmpl.ExecuteTemplate(w, "edit_product.html", data)
}

type SupplierSales struct {
	Supplier  string
	Qty       int
	RevenueRs int
	CostRs    int
	ProfitRs  int
}

type reportsSuppliersPageData struct {
	Rows        []SupplierSales
	FromDate    string
	ToDate      string
	GrandQty    int
	GrandRev    int
	GrandCost   int
	GrandProfit int
}

func reportsSuppliersHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to := parseDateRange(r)
		fromISO := from.Format("2006-01-02")
		toISO := to.Format("2006-01-02")

		rows, err := db.Query(`
			SELECT COALESCE(s.name, 'Unassigned'),
			       SUM(ti.quantity),
			       SUM(ti.line_total_rupees),
			       COALESCE(SUM(ti.quantity * p.cost_price_rupees), 0)
			FROM transaction_items ti
			JOIN products p ON p.id = ti.product_id
			JOIN transactions t ON t.id = ti.transaction_id
			LEFT JOIN suppliers s ON s.id = p.supplier_id
			WHERE date(t.created_at, '+5 hours') BETWEEN ? AND ?
			GROUP BY s.name
			ORDER BY SUM(ti.line_total_rupees) DESC
		`, fromISO, toISO)
		if err != nil {
			log.Printf("supplier report query: %v", err)
			http.Error(w, "failed to load report", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		data := reportsSuppliersPageData{FromDate: fromISO, ToDate: toISO}
		for rows.Next() {
			var cs SupplierSales
			var cost sql.NullInt64
			if err := rows.Scan(&cs.Supplier, &cs.Qty, &cs.RevenueRs, &cost); err != nil {
				log.Printf("supplier report scan: %v", err)
				http.Error(w, "failed to load report", http.StatusInternalServerError)
				return
			}
			if cost.Valid {
				cs.CostRs = int(cost.Int64)
			}
			cs.ProfitRs = cs.RevenueRs - cs.CostRs
			data.Rows = append(data.Rows, cs)
			data.GrandQty += cs.Qty
			data.GrandRev += cs.RevenueRs
			data.GrandCost += cs.CostRs
			data.GrandProfit += cs.ProfitRs
		}
		if err := rows.Err(); err != nil {
			log.Printf("supplier report rows: %v", err)
			http.Error(w, "failed to load report", http.StatusInternalServerError)
			return
		}

		if err := tmpl.ExecuteTemplate(w, "reports_suppliers.html", data); err != nil {
			log.Printf("render reports_suppliers: %v", err)
		}
	}
}

type ItemwiseRow struct {
	ProductID int
	Name      string
	Qty       int
	SaleRs    int
	CostRs    int
	MarginRs  int
	MarginPct string // "12.5%" or "—" when sale is 0
	Note      string // "" | "est." (cost estimated from current) | "incomplete" (some lines have no cost at all)
}

type itemwiseTotals struct {
	Qty       int
	SaleRs    int
	CostRs    int
	MarginRs  int
	NotedRows int
}

// queryItemwise is shared by the HTML page and the CSV export.
// Cost per line = the checkout-time snapshot; for rows written before the
// snapshot column existed it falls back to the product's CURRENT cost and the
// row is marked "est." — see decision 18.
func queryItemwise(db *sql.DB, fromISO, toISO string) ([]ItemwiseRow, itemwiseTotals, error) {
	rows, err := db.Query(`
		SELECT p.id, p.name,
		       SUM(ti.quantity),
		       SUM(ti.line_total_rupees),
		       COALESCE(SUM(ti.quantity * COALESCE(ti.cost_price_at_sale_rupees, p.cost_price_rupees)), 0),
		       SUM(CASE WHEN ti.cost_price_at_sale_rupees IS NULL AND p.cost_price_rupees IS NOT NULL THEN 1 ELSE 0 END),
		       SUM(CASE WHEN ti.cost_price_at_sale_rupees IS NULL AND p.cost_price_rupees IS NULL THEN 1 ELSE 0 END)
		FROM transaction_items ti
		JOIN products p ON p.id = ti.product_id
		JOIN transactions t ON t.id = ti.transaction_id
		WHERE date(t.created_at, '+5 hours') BETWEEN ? AND ?
		GROUP BY p.id
		ORDER BY SUM(ti.line_total_rupees) DESC
	`, fromISO, toISO)
	if err != nil {
		return nil, itemwiseTotals{}, err
	}
	defer rows.Close()

	var out []ItemwiseRow
	var totals itemwiseTotals
	for rows.Next() {
		var it ItemwiseRow
		var estLines, unknownLines int
		if err := rows.Scan(&it.ProductID, &it.Name, &it.Qty, &it.SaleRs, &it.CostRs, &estLines, &unknownLines); err != nil {
			return nil, itemwiseTotals{}, err
		}
		it.MarginRs = it.SaleRs - it.CostRs
		if it.SaleRs != 0 {
			it.MarginPct = fmt.Sprintf("%.1f%%", 100*float64(it.MarginRs)/float64(it.SaleRs))
		} else {
			it.MarginPct = "—"
		}
		if unknownLines > 0 {
			it.Note = "incomplete"
		} else if estLines > 0 {
			it.Note = "est."
		}
		if it.Note != "" {
			totals.NotedRows++
		}
		totals.Qty += it.Qty
		totals.SaleRs += it.SaleRs
		totals.CostRs += it.CostRs
		totals.MarginRs += it.MarginRs
		out = append(out, it)
	}
	return out, totals, rows.Err()
}

type reportsItemwisePageData struct {
	Rows     []ItemwiseRow
	FromDate string
	ToDate   string
	Totals   itemwiseTotals
	GrandPct string
}

func reportsItemwiseHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to := parseDateRange(r)
		fromISO := from.Format("2006-01-02")
		toISO := to.Format("2006-01-02")

		items, totals, err := queryItemwise(db, fromISO, toISO)
		if err != nil {
			log.Printf("itemwise report query: %v", err)
			http.Error(w, "failed to load report", http.StatusInternalServerError)
			return
		}

		data := reportsItemwisePageData{Rows: items, FromDate: fromISO, ToDate: toISO, Totals: totals, GrandPct: "—"}
		if totals.SaleRs != 0 {
			data.GrandPct = fmt.Sprintf("%.1f%%", 100*float64(totals.MarginRs)/float64(totals.SaleRs))
		}
		if err := tmpl.ExecuteTemplate(w, "reports_itemwise.html", data); err != nil {
			log.Printf("render reports_itemwise: %v", err)
		}
	}
}

func reportsItemwiseCSVHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to := parseDateRange(r)
		fromISO := from.Format("2006-01-02")
		toISO := to.Format("2006-01-02")

		items, totals, err := queryItemwise(db, fromISO, toISO)
		if err != nil {
			log.Printf("itemwise csv query: %v", err)
			http.Error(w, "failed to export", http.StatusInternalServerError)
			return
		}

		filename := fmt.Sprintf("itemwise-%s-to-%s.csv", fromISO, toISO)
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

		cw := csv.NewWriter(w)
		cw.Write([]string{"Product", "Qty", "Sale (Rs.)", "Cost (Rs.)", "Margin (Rs.)", "P/L %", "Cost Note"})
		for _, it := range items {
			cw.Write([]string{
				it.Name,
				strconv.Itoa(it.Qty),
				strconv.Itoa(it.SaleRs),
				strconv.Itoa(it.CostRs),
				strconv.Itoa(it.MarginRs),
				it.MarginPct,
				it.Note,
			})
		}
		grandPct := "—"
		if totals.SaleRs != 0 {
			grandPct = fmt.Sprintf("%.1f%%", 100*float64(totals.MarginRs)/float64(totals.SaleRs))
		}
		cw.Write([]string{"TOTAL", strconv.Itoa(totals.Qty), strconv.Itoa(totals.SaleRs), strconv.Itoa(totals.CostRs), strconv.Itoa(totals.MarginRs), grandPct, ""})
		cw.Flush()
	}
}
