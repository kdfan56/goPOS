package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
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

// Loaded once at startup; receipts display Pakistan wall-clock time.
var storeLocation *time.Location

// lowStockThreshold is the stock count below which a product is flagged as "low".
// Affects: SQL filter on /products?low=1, the banner count, the row colour CSS in
// products.html (rendered via .Threshold from the page data), and the orange
// "low" colour in templates/scan.html (still hardcoded — sync manually if changed).
const lowStockThreshold = 10

type Product struct {
	ID          int
	Barcode     string
	Name        string
	PriceRupees int
	Stock       int
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

	// SQLite ignores REFERENCES clauses unless this is on. Do not remove.
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

	// One-off migration: add supplier_id to existing receiving_sessions tables.
	// SQLite ALTER TABLE has no IF NOT EXISTS; we ignore the "duplicate column" error.
	if _, err := db.Exec(`ALTER TABLE receiving_sessions ADD COLUMN supplier_id INTEGER REFERENCES suppliers(id)`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			log.Fatalf("migrate receiving_sessions.supplier_id: %v", err)
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
	mux.HandleFunc("GET /pos", posHandler(tmpl))
	mux.HandleFunc("GET /price-check", priceCheckPageHandler(tmpl))
	mux.HandleFunc("POST /pos/scan", scanHandler(db))
	mux.HandleFunc("GET /scan", scanPageHandler(tmpl))
	mux.HandleFunc("POST /pos/checkout", checkoutHandler(db))
	mux.HandleFunc("GET /receipt/{id}", receiptHandler(db, tmpl))
	mux.HandleFunc("POST /stock/update", stockUpdateHandler(db))
	mux.HandleFunc("GET /reports", reportsHandler(db, tmpl))
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

// ctxKey is a private type for context keys so values can't collide with keys
// set elsewhere. roleKey carries the authenticated role ("admin"/"cashier") from
// requireAuth down to handlers — the dashboard uses it to send cashiers to /pos.
type ctxKey int

const roleKey ctxKey = iota

// requireAuth gates every request behind HTTP Basic Auth and a role.
// Two credential pairs: admin (full access) and cashier (POS + price check only).
// Whichever pair matches sets the role; cashiers are then restricted by cashierAllowed.
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

// credMatch compares supplied credentials against an expected pair in constant
// time, so an attacker can't learn the username/password from response timing.
func credMatch(u, p, wantUser, wantPass string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(u), []byte(wantUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(p), []byte(wantPass)) == 1
	return userOK && passOK
}

// cashierAllowed is the allow-list of paths a cashier role may reach. Everything
// not listed here (products, stock, reports, receiving) is admin-only. Keep this
// in sync with the access matrix in NOTES.md when routes are added.
func cashierAllowed(path string) bool {
	switch path {
	case "/", "/pos", "/price-check", "/pos/scan", "/pos/checkout":
		return true
	}
	// Receipts are /receipt/{id} — cashier prints what they sell.
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

	// Wrap seeding in a tx so each seed product + its initial movement row land
	// atomically. Keeps the audit trail consistent with user-added products —
	// otherwise seeded items would have no history and look "empty" on /scan.
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
		// Normalize: strip everything that isn't a digit. Handles whitespace,
		// scanner-format delimiters, control chars — anything non-digit gets dropped.
		// Read side stays permissive on length (lookup just won't match anything).
		req.Barcode = normalizeBarcode(req.Barcode)
		if req.Barcode == "" {
			writeJSONError(w, http.StatusBadRequest, "barcode required")
			return
		}

		var p Product
		err := db.QueryRow(
			"SELECT id, barcode, name, price_rupees, stock FROM products WHERE barcode = ?",
			req.Barcode,
		).Scan(&p.ID, &p.Barcode, &p.Name, &p.PriceRupees, &p.Stock)

		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "barcode not found")
			return
		}
		if err != nil {
			log.Printf("scan query: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "lookup failed")
			return
		}

		// Recent additions for the receiving-context display on /scan.
		// Filter delta > 0 so sales never leak into this view — receivers only
		// care about stock that *came in*. Non-empty slice initializer keeps
		// the JSON shape as [] rather than null when there's no history yet.
		additions := recentAdditions(db, p.ID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":               p.ID,
			"barcode":          p.Barcode,
			"name":             p.Name,
			"price_rupees":     p.PriceRupees,
			"stock":            p.Stock,
			"recent_additions": additions,
		})
	}
}

type recentAddition struct {
	Delta     int    `json:"delta"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"` // formatted in store-local time, ready to render
}

func recentAdditions(db *sql.DB, productID int) []recentAddition {
	out := []recentAddition{} // not nil — JSON should always be an array
	rows, err := db.Query(
		// id DESC is a tiebreaker: created_at has second-resolution, so two
		// adjustments inside the same second would otherwise return in undefined
		// order. id is monotonic autoincrement, so newer rows always sort higher.
		`SELECT delta, reason, created_at
		 FROM stock_movements
		 WHERE product_id = ? AND delta > 0
		 ORDER BY created_at DESC, id DESC
		 LIMIT 5`,
		productID,
	)
	if err != nil {
		// Non-fatal: lookup still returns the product. Logged for visibility.
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
			quantity    int
			stockBefore int // captured pre-update so the movement row can record new_stock
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
				"SELECT name, price_rupees, stock FROM products WHERE id = ?",
				item.ProductID,
			).Scan(&p.name, &p.price, &p.stockBefore)
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
				"INSERT INTO transaction_items (transaction_id, product_id, quantity, unit_price_rupees, line_total_rupees) VALUES (?, ?, ?, ?, ?)",
				txnID, p.id, p.quantity, p.price, p.price*p.quantity,
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
	Date        string // ISO YYYY-MM-DD, used for map keys and CSS
	Display     string // human-friendly label like "Today (Fri 15 May)"
	IsToday     bool
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
}

func reportsHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Today, in Pakistan local time. Used both for the SQL window lower bound
		// and to label each row.
		nowLocal := time.Now().In(storeLocation)
		today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, storeLocation)
		todayISO := today.Format("2006-01-02")

		// Pre-build a scaffold of the last 7 days so days with zero sales still appear.
		daysMap := make(map[string]*DailySales, 7)
		ordered := make([]string, 0, 7)
		for i := 0; i < 7; i++ {
			d := today.AddDate(0, 0, -i)
			iso := d.Format("2006-01-02")
			ordered = append(ordered, iso)

			var display string
			switch i {
			case 0:
				display = "Today (" + d.Format("Mon 2 Jan") + ")"
			case 1:
				display = "Yesterday (" + d.Format("Mon 2 Jan") + ")"
			default:
				display = d.Format("Mon 2 Jan 2006")
			}
			daysMap[iso] = &DailySales{Date: iso, Display: display, IsToday: i == 0}
		}

		// Group transactions by Pakistan-local date. SQLite stores created_at as UTC;
		// `date(col, '+5 hours')` shifts each row into Pakistan local time before truncating.
		// Pakistan does not observe DST so a fixed offset is safe.
		rows, err := db.Query(`
			SELECT date(created_at, '+5 hours') AS day,
			       SUM(CASE WHEN payment_method = 'cash' THEN total_rupees ELSE 0 END) AS cash_total,
			       SUM(CASE WHEN payment_method = 'card' THEN total_rupees ELSE 0 END) AS card_total,
			       SUM(total_rupees) AS day_total,
			       COUNT(*) AS txn_count
			FROM transactions
			WHERE date(created_at, '+5 hours') >= date(?, '-6 days')
			GROUP BY day
		`, todayISO)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var day string
			var cash, card, total, count int
			if err := rows.Scan(&day, &cash, &card, &total, &count); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if ds, ok := daysMap[day]; ok {
				ds.CashRupees = cash
				ds.CardRupees = card
				ds.TotalRupees = total
				ds.TxnCount = count
			}
		}

		data := ReportsPageData{Days: make([]DailySales, 0, len(ordered))}
		for _, iso := range ordered {
			ds := *daysMap[iso]
			data.Days = append(data.Days, ds)
			data.GrandCash += ds.CashRupees
			data.GrandCard += ds.CardRupees
			data.GrandTotal += ds.TotalRupees
			data.GrandCount += ds.TxnCount
		}

		if err := tmpl.ExecuteTemplate(w, "reports.html", data); err != nil {
			log.Printf("render reports: %v", err)
		}
	}
}

type dashboardTopItem struct {
	Name      string
	Qty       int
	RevenueRs string // preformatted with thousands separators
}

type dashboardData struct {
	TodaySalesRs string
	TodayCount   int
	MonthLabel   string // e.g. "May 2026"
	MonthSalesRs string
	LowStockCount int
	TopItems      []dashboardTopItem
	ReportDays    []DailySales
	GrandCash     int
	GrandCard     int
	GrandTotal    int
	GrandCount    int
}

// formatRupees renders a whole-rupee amount with thousands separators
// (12450 -> "12,450"). Done in Go so the template can stay dumb.
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
		// Cashiers don't see manager numbers — bounce them to the till.
		if role, _ := r.Context().Value(roleKey).(string); role == "cashier" {
			http.Redirect(w, r, "/pos", http.StatusSeeOther)
			return
		}

		// Pakistan-local "today" and current month, matching the reports convention
		// (SQLite stores created_at as UTC; '+5 hours' shifts to local before truncating).
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

		// Top 5 products by units sold over the last 7 local days.
		rows, err := db.Query(
			`SELECT p.name, SUM(ti.quantity) AS qty, SUM(ti.line_total_rupees) AS revenue
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
			if err := rows.Scan(&it.Name, &it.Qty, &revenue); err != nil {
				log.Printf("dashboard top scan: %v", err)
				http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
				return
			}
			it.RevenueRs = formatRupees(revenue)
			top = append(top, it)
		}

		var lowStockCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM products WHERE stock < ?`, lowStockThreshold,
		).Scan(&lowStockCount); err != nil {
			log.Printf("dashboard low stock query: %v", err)
			http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
			return
		}

		// 7-day sales breakdown — same query as reportsHandler, same scaffold/merge pattern.
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

		// Read old stock first so we can compute the signed delta and detect not-found
		// in one shot (no separate RowsAffected probe needed).
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

		// If the new value equals the old, skip both the UPDATE and the movement
		// row — no real change happened, no point polluting the audit log.
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

		// Return the refreshed additions list so /scan can re-render it without
		// a second roundtrip. Cheap indexed query, fires after commit.
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
	SupplierName string // "" if no supplier linked
	StartedAt    string // formatted store-local, ready to render
	ItemCount    int    // distinct products scanned in
	TotalQty     int    // sum of all qtys
}

type receivingListData struct {
	Sessions  []ReceivingSession
	Suppliers []Supplier // active suppliers, for the "Start session" dropdown
	Error     string
}

// loadActiveSuppliers returns all active suppliers ordered by name.
// Used for dropdowns on the receiving list page.
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

// renderReceivingList loads the active sessions and renders the list page.
// Shared by the GET handler and the failed-create path so there's one place
// that knows how to build this page (errMsg is "" for the normal GET).
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

		// PRG: 303 straight into the new session's page so scanning can begin.
		http.Redirect(w, r, fmt.Sprintf("/receiving/%d", id), http.StatusSeeOther)
	}
}

type ReceivingItem struct {
	ProductID int    `json:"product_id"`
	Barcode   string `json:"barcode"`
	Name      string `json:"name"`
	Qty       int    `json:"qty"`
	Stock     int    `json:"stock"` // current product stock, for receiver context
}

type receivingDetailData struct {
	ID           int
	Label        string
	SupplierName string // "" if no supplier linked
	StartedAt    string
	FinalizedAt  string // "" while active
	Status       string
	TotalQty     int
	Error        string      // surfaced from the ?err= query param
	ItemsJSON    template.JS // marshaled []ReceivingItem, rendered into the page's JS
}

// loadReceivingItems returns a session's items (joined to product info), ordered
// by name, plus the summed qty. Used by the detail page and the scan/item APIs.
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

	items := []ReceivingItem{} // non-nil so JSON renders as [] not null
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

// receivingSessionStatus reports a session's status and whether it exists.
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

// writeReceivingItems writes a session's current items as the JSON response
// shared by the scan and item-adjust endpoints. scanned is the just-scanned
// product name (for a status message) or "" when not applicable.
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

		var productID int
		var name string
		err = db.QueryRow("SELECT id, name FROM products WHERE barcode = ?", barcode).Scan(&productID, &name)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "barcode not found")
			return
		}
		if err != nil {
			log.Printf("receiving scan lookup: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "lookup failed")
			return
		}

		// Upsert: first scan inserts qty 1, repeat scans bump it.
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
			Op        string `json:"op"` // "inc" | "dec" | "remove"
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
			// Decrement, then drop the row if it hit zero — qty 0 is the "remove me"
			// signal (decision 12). Two statements, one transaction.
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

		// Only an active session can be deleted; a finalized one changed stock and
		// is permanent. If it's gone or finalized, just bounce back to the list.
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

		// Items first — they reference the session (FK), so the parent can't go
		// while children point at it.
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

		// Must still be active. A finalized session is already committed — just show it.
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

		// Read all items first — can't run UPDATEs on this tx while the rows cursor
		// is still open on the same connection.
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

		// Block an empty finalize — nothing to commit.
		if len(lines) == 0 {
			http.Redirect(w, r, fmt.Sprintf("/receiving/%d?err=empty", id), http.StatusSeeOther)
			return
		}

		// Add each qty to stock and log one movement per item. RETURNING gives the
		// post-update stock atomically, so the audit snapshot is exact even if
		// another writer touched the same product between sessions.
		for _, l := range lines {
			var newStock int
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

// normalizeBarcode strips every non-digit character. Defensive against
// scanners that return delimiters or control chars alongside the actual digits.
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

// validateBarcodeLength enforces the standard retail lengths.
// See decisions/08-barcode-standardization.md for the rationale.
func validateBarcodeLength(s string) error {
	n := len(s)
	if n != 8 && n != 12 && n != 13 && n != 14 {
		return fmt.Errorf("barcode must be 8, 12, 13, or 14 digits (got %d)", n)
	}
	return nil
}

// recordMovement logs a stock change to the audit table. Always call it from
// inside the same transaction as the UPDATE that changed products.stock — if
// the insert fails, the stock change rolls back with it. See decisions/09.
// reason: 'sale' | 'manual_adjust' | 'initial' | 'receiving'
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

// ProductFormData backs the new_product.html template. Fields are strings
// (not ints) so the user's raw input is preserved verbatim across a validation
// failure — including malformed values they need to see to fix.
type ProductFormData struct {
	Barcode     string
	Name        string
	PriceRupees string
	Stock       string
	Return      string // where to redirect on success; "" → /products
	Error       string
}

// validateReturnPath protects against open-redirect attacks: only paths starting
// with a single "/" are allowed. Rejects absolute URLs (https://evil.com),
// protocol-relative URLs (//evil.com), and empty values.
func validateReturnPath(s string) string {
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return ""
	}
	return s
}

func newProductFormHandler(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// If a return value was supplied but fails validation, strip it from the URL
		// and redirect. This keeps a malicious value like ?return=https://evil.com from
		// sitting in the browser address bar, which would be alarming even though the
		// server-side defense already neutralises it for the redirect on submit.
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

		// Pre-fill from query params when the user arrived here via the "Add this product"
		// link on a /scan 404 — barcode is the scanned digits, return is the path to send
		// them back to after a successful add.
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
		// Normalize+validate the barcode against the store standard.
		// We rewrite form.Barcode so the (now-clean) value is what gets re-rendered on later errors
		// and what gets stored on success.
		normalized := normalizeBarcode(form.Barcode)
		if normalized == "" {
			fail("Barcode must contain digits")
			return
		}
		if err := validateBarcodeLength(normalized); err != nil {
			fail(strings.ToUpper(err.Error()[:1]) + err.Error()[1:])
			return
		}
		form.Barcode = normalized
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

		res, err := tx.ExecContext(ctx,
			"INSERT INTO products (barcode, name, price_rupees, stock) VALUES (?, ?, ?, ?)",
			form.Barcode, form.Name, price, stock,
		)
		if err != nil {
			// SQLite UNIQUE-constraint error message contains "UNIQUE constraint failed".
			if strings.Contains(err.Error(), "UNIQUE") {
				fail("A product with that barcode already exists")
				return
			}
			log.Printf("insert product: %v", err)
			fail("Failed to add product")
			return
		}
		// Skip the movement row when the new product starts at zero stock —
		// no change happened, no point in a zero-delta entry.
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

		// PRG (Post/Redirect/Get): 303 prevents refresh-resubmits and accidental duplicates.
		target := form.Return
		if target == "" {
			target = "/products"
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

type productsPageData struct {
	Products  []Product
	LowCount  int
	LowOnly   bool
	Threshold int
}

func productsHandler(db *sql.DB, tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lowOnly := r.URL.Query().Get("low") == "1"

		query := "SELECT id, barcode, name, price_rupees, stock FROM products"
		args := []any{}
		if lowOnly {
			query += " WHERE stock < ?"
			args = append(args, lowStockThreshold)
		}
		query += " ORDER BY name"

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var products []Product
		for rows.Next() {
			var p Product
			if err := rows.Scan(&p.ID, &p.Barcode, &p.Name, &p.PriceRupees, &p.Stock); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			products = append(products, p)
		}

		// Always count low-stock items so the banner can show even when not filtering.
		var lowCount int
		if err := db.QueryRow("SELECT COUNT(*) FROM products WHERE stock < ?", lowStockThreshold).Scan(&lowCount); err != nil {
			log.Printf("count low stock: %v", err)
			// Non-fatal — banner just won't show. Page still renders.
		}

		data := productsPageData{
			Products:  products,
			LowCount:  lowCount,
			LowOnly:   lowOnly,
			Threshold: lowStockThreshold,
		}
		if err := tmpl.ExecuteTemplate(w, "products.html", data); err != nil {
			log.Printf("render products: %v", err)
		}
	}
}
