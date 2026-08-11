// demoseed fills an EMPTY POS database with believable history so that the
// dashboard and the reports have something to draw. It is a development tool.
// It is not part of the server binary and it never runs in the store.
//
// WARNING: this tool writes historical rows. It does NOT change product stock
// and it does NOT write to stock_movements. That is deliberate, and it is the
// same call the Phase 5 product import made. Today's stock number is already
// today's truth. Replaying a year of sales against it would drive thousands of
// SKUs far below zero and make the reorder list useless. The rule "every stock
// change must be logged" still holds, because this tool changes no stock.
//
// Usage:
//
//	go run cmd/demoseed/main.go --db pos.db
//	go run cmd/demoseed/main.go --db pos.db --days 400 --per-day 120 --force
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"time"

	_ "modernc.org/sqlite"
)

// A sale that demoseed remembers for a short time, so that a later day can
// refund it. Returns in a grocery are nearly always against an earlier receipt.
type pastSale struct {
	id       int64
	when     time.Time // local store time
	method   string
	lines    []saleLine
	refunded bool
}

type saleLine struct {
	productID int
	qty       int
	unitPrice int
	cost      sql.NullInt64
}

// A product that is worth putting in a basket: it has stock and a real price.
type poolItem struct {
	id    int
	price int
	cost  sql.NullInt64
}

func main() {
	dbPath := flag.String("db", "pos.db", "path to SQLite database")
	days := flag.Int("days", 400, "how many days of history to generate, ending today")
	perDay := flag.Int("per-day", 120, "average sales per day (a weekday; weekends get more)")
	returnRate := flag.Float64("return-rate", 0.012, "fraction of sales that later come back as a refund")
	seed := flag.Int64("seed", 1, "random seed, so a run is repeatable")
	force := flag.Bool("force", false, "write even if the database already holds transactions")
	flag.Parse()

	loc, err := time.LoadLocation("Asia/Karachi")
	if err != nil {
		log.Fatalf("load timezone: %v", err)
	}

	db, err := sql.Open("sqlite", *dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var existing int
	if err := db.QueryRow("SELECT COUNT(*) FROM transactions").Scan(&existing); err != nil {
		log.Fatalf("count transactions: %v", err)
	}
	if existing > 0 && !*force {
		log.Fatalf("database already holds %d transactions. Run with --force to add more anyway.", existing)
	}

	rng := rand.New(rand.NewSource(*seed))

	pool, items := loadPool(db, rng)
	log.Printf("basket pool: %d sellable products, %d weighted entries", len(items), len(pool))

	supplierIDs := ensureSuppliers(db, rng, items)

	tx, err := db.Begin()
	if err != nil {
		log.Fatalf("begin: %v", err)
	}

	nowLocal := time.Now().In(loc)
	today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)
	start := today.AddDate(0, 0, -(*days - 1))

	stats := generateSales(tx, rng, loc, start, today, *perDay, *returnRate, pool)
	sessions := generateReceiving(tx, rng, loc, start, today, supplierIDs, items)

	if err := tx.Commit(); err != nil {
		log.Fatalf("commit: %v", err)
	}

	log.Printf("done. sales=%d returns=%d items=%d receiving_sessions=%d",
		stats.sales, stats.returns, stats.items, sessions)
	log.Printf("sale money=Rs %d, average basket=Rs %d",
		stats.saleRupees, stats.saleRupees/max(stats.sales, 1))
	log.Printf("range: %s .. %s (store local time)",
		start.Format("2006-01-02"), today.Format("2006-01-02"))
}

// loadPool builds the list that a basket draws from. A product appears in the
// pool more than one time when it is cheap, because a real shopper buys many
// cheap things and few expensive things. Without this weighting every basket
// would average the catalogue price of Rs 568, and a one-line basket would
// already be larger than the store's real average basket of about Rs 545.
func loadPool(db *sql.DB, rng *rand.Rand) ([]poolItem, []poolItem) {
	rows, err := db.Query(
		`SELECT id, price_rupees, cost_price_rupees
		   FROM products
		  WHERE stock > 0 AND price_rupees > 0`)
	if err != nil {
		log.Fatalf("load products: %v", err)
	}
	defer rows.Close()

	var items []poolItem
	var pool []poolItem
	for rows.Next() {
		var it poolItem
		if err := rows.Scan(&it.id, &it.price, &it.cost); err != nil {
			log.Fatalf("scan product: %v", err)
		}
		items = append(items, it)
		for i := 0; i < priceWeight(it.price); i++ {
			pool = append(pool, it)
		}
	}
	if len(pool) == 0 {
		log.Fatal("no sellable products found. Import the catalogue first.")
	}
	return pool, items
}

// priceWeight gives a cheap item more chances to land in a basket.
// Rs 20 tea bag -> 250 entries. Rs 500 shampoo -> 10. Rs 20,000 appliance -> 1.
//
// The two constants are tuned, not guessed. The catalogue median price is
// Rs 230. These weights pull the expected unit price down to about Rs 85,
// which multiplied by the average line count and the average quantity gives an
// average basket near Rs 545 — the real figure on the old iPOS dashboard.
// Change a constant here and the average basket moves. Measure after you do.
func priceWeight(price int) int {
	w := 5000 / price
	if w < 1 {
		return 1
	}
	if w > 400 {
		return 400
	}
	return w
}

// ensureSuppliers creates a few suppliers and gives roughly half of the
// catalogue a supplier, so that the supplier and purchase reports are not empty.
func ensureSuppliers(db *sql.DB, rng *rand.Rand, items []poolItem) []int64 {
	var have int
	if err := db.QueryRow("SELECT COUNT(*) FROM suppliers").Scan(&have); err != nil {
		log.Fatalf("count suppliers: %v", err)
	}
	if have > 0 {
		rows, err := db.Query("SELECT id FROM suppliers WHERE active = 1")
		if err != nil {
			log.Fatalf("load suppliers: %v", err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				log.Fatalf("scan supplier: %v", err)
			}
			ids = append(ids, id)
		}
		return ids
	}

	names := []struct{ name, contact, phone string }{
		{"Unilever Pakistan", "Bilal Ahmed", "0300-1234567"},
		{"Nestle Distributors", "Farhan Iqbal", "0321-2345678"},
		{"National Foods Agency", "Usman Tariq", "0333-3456789"},
		{"Colgate Palmolive Dist", "Adnan Sheikh", "0345-4567890"},
		{"Shan Foods Supplier", "Kamran Ali", "0301-5678901"},
		{"Tapal Tea Wholesale", "Hamza Rauf", "0322-6789012"},
		{"Local Bakery Supply", "Rizwan Khan", "0334-7890123"},
		{"General Merchandise Co", "Saad Mahmood", "0346-8901234"},
	}

	tx, err := db.Begin()
	if err != nil {
		log.Fatalf("begin suppliers: %v", err)
	}
	var ids []int64
	for _, n := range names {
		res, err := tx.Exec(
			`INSERT INTO suppliers (name, phone, contact_person, active) VALUES (?, ?, ?, 1)`,
			n.name, n.phone, n.contact)
		if err != nil {
			log.Fatalf("insert supplier: %v", err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}

	// Give about half of the sellable catalogue a supplier. The rest stay
	// NULL on purpose: the supplier report has an "Unassigned" row and it
	// should have something in it.
	stmt, err := tx.Prepare("UPDATE products SET supplier_id = ? WHERE id = ?")
	if err != nil {
		log.Fatalf("prepare supplier update: %v", err)
	}
	defer stmt.Close()
	assigned := 0
	for _, it := range items {
		if rng.Float64() > 0.55 {
			continue
		}
		if _, err := stmt.Exec(ids[rng.Intn(len(ids))], it.id); err != nil {
			log.Fatalf("assign supplier: %v", err)
		}
		assigned++
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("commit suppliers: %v", err)
	}
	log.Printf("created %d suppliers, assigned %d products", len(ids), assigned)
	return ids
}

type saleStats struct {
	sales      int
	returns    int
	items      int
	saleRupees int
}

// basketShape decides how many lines a basket holds. Most people buy a handful
// of things. A few fill a trolley. The list is the distribution, written out
// rather than computed, because it is easier to read and easier to tune.
var basketShape = []int{1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 4, 4, 5, 5, 6, 7, 8, 10, 12}

func generateSales(
	tx *sql.Tx, rng *rand.Rand, loc *time.Location,
	start, today time.Time, perDay int, returnRate float64,
	pool []poolItem,
) saleStats {
	insertTxn, err := tx.Prepare(
		`INSERT INTO transactions (total_rupees, payment_method, kind, original_transaction_id, created_at)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		log.Fatalf("prepare transaction insert: %v", err)
	}
	defer insertTxn.Close()

	insertItem, err := tx.Prepare(
		`INSERT INTO transaction_items
		   (transaction_id, product_id, quantity, unit_price_rupees, line_total_rupees, cost_price_at_sale_rupees)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		log.Fatalf("prepare item insert: %v", err)
	}
	defer insertItem.Close()

	var stats saleStats
	var recent []pastSale // rolling window of sales that a refund can point at

	full := fullDayClock()
	partial := partialDayClock(time.Now().In(loc))

	totalDays := int(today.Sub(start).Hours()/24) + 1
	for d := 0; d < totalDays; d++ {
		day := start.AddDate(0, 0, d)

		// Today is not over yet. Use a clock that stops at the current hour and
		// scale the customer count down to match.
		clock := full
		partOfDay := 1.0
		if day.Equal(today) {
			clock = partial
			partOfDay = partial.fraction()
			if !clock.open() {
				break // the store has not opened today
			}
		}

		// Drop sales that are too old to be refunded, so the window stays small.
		cutoff := day.AddDate(0, 0, -21)
		kept := recent[:0]
		for _, s := range recent {
			if s.when.After(cutoff) {
				kept = append(kept, s)
			}
		}
		recent = kept

		count := int(float64(salesForDay(rng, day, start, today, perDay)) * partOfDay)
		for i := 0; i < count; i++ {
			sale := writeSale(insertTxn, insertItem, rng, loc, day, clock, pool, &stats)
			recent = append(recent, sale)
		}

		// Refunds for today, drawn from earlier receipts in the window.
		wanted := int(float64(count) * returnRate)
		if rng.Float64() < float64(count)*returnRate-float64(wanted) {
			wanted++
		}
		for i := 0; i < wanted && len(recent) > 0; i++ {
			idx := rng.Intn(len(recent))
			orig := &recent[idx]
			// A receipt is refunded one time only. That keeps the generated
			// data inside the same rule the return screen enforces.
			if orig.refunded || len(orig.lines) == 0 {
				continue
			}
			// Refund a receipt from an EARLIER day only. A refund must never
			// carry a timestamp before its own sale, and both timestamps get a
			// random clock hour. Same-day pairs would sometimes come out in the
			// wrong order. An earlier day also matches how a grocery works:
			// the customer comes back on a later visit.
			if !orig.when.Before(day) {
				continue
			}
			orig.refunded = true
			writeReturn(insertTxn, insertItem, rng, loc, day, clock, *orig, &stats)
		}
	}
	return stats
}

// salesForDay varies the customer count so that a chart has a shape.
// A weekend is busier. The store also grows slowly across the period.
func salesForDay(rng *rand.Rand, day, start, today time.Time, perDay int) int {
	base := float64(perDay)

	switch day.Weekday() {
	case time.Friday:
		base *= 1.15
	case time.Saturday:
		base *= 1.35
	case time.Sunday:
		base *= 1.30
	case time.Monday:
		base *= 0.85
	}

	// Slow growth from the first day to the last: about 20 percent over the
	// whole range, so the monthly chart trends up instead of sitting flat.
	span := today.Sub(start).Hours()
	if span > 0 {
		progress := day.Sub(start).Hours() / span
		base *= 0.9 + 0.3*progress
	}

	// Ramadan-style and holiday spikes are not modelled. Plain noise is enough.
	base *= 0.85 + rng.Float64()*0.3

	n := int(base)
	if n < 1 {
		return 1
	}
	return n
}

// hourWeights spreads transactions across the trading day in store local time.
// The store opens at 09:00 and closes at 22:00. The evening is the busy part of
// a grocery day.
var hourWeights = map[int]int{
	9: 3, 10: 5, 11: 6, 12: 7, 13: 6, 14: 5, 15: 5,
	16: 7, 17: 9, 18: 12, 19: 14, 20: 13, 21: 8,
}

// dayClock picks a believable time of day. The last day of the range is only
// partly over, so the clock also carries a cap. demoseed must never write a
// transaction with a timestamp in the future.
type dayClock struct {
	hours   []int // weighted hour list, already limited to the allowed hours
	capHour int   // the hour that the cap falls in, or -1 for a whole day
	capMin  int   // minutes available inside capHour
}

// fullDayClock allows every trading hour. Use it for any day before today.
func fullDayClock() dayClock {
	return dayClock{hours: hourPoolUpTo(23), capHour: -1}
}

// partialDayClock allows only the hours that have already happened.
func partialDayClock(now time.Time) dayClock {
	return dayClock{hours: hourPoolUpTo(now.Hour()), capHour: now.Hour(), capMin: now.Minute()}
}

func hourPoolUpTo(maxHour int) []int {
	var p []int
	for h, w := range hourWeights {
		if h > maxHour {
			continue
		}
		for i := 0; i < w; i++ {
			p = append(p, h)
		}
	}
	return p
}

// open reports whether the clock has any hour to give. Before 09:00 the store
// has not opened, so today gets no transactions at all.
func (c dayClock) open() bool { return len(c.hours) > 0 }

// fraction is how much of a normal trading day this clock covers. The day's
// customer count is scaled by it, so a dashboard opened at 11:00 shows a part
// day rather than a suspiciously complete one.
func (c dayClock) fraction() float64 {
	total := 0
	for _, w := range hourWeights {
		total += w
	}
	if total == 0 {
		return 0
	}
	return float64(len(c.hours)) / float64(total)
}

func (c dayClock) pick(rng *rand.Rand, day time.Time, loc *time.Location) time.Time {
	hour := c.hours[rng.Intn(len(c.hours))]
	minute := rng.Intn(60)
	second := rng.Intn(60)
	if hour == c.capHour {
		if c.capMin <= 0 {
			// The cap hour has only just begun. Pin to the top of the hour,
			// SECONDS INCLUDED — a random second here is up to 59 seconds in
			// the future, which breaks this type's whole promise.
			minute, second = 0, 0
		} else {
			// rng.Intn(capMin) is strictly below the current minute, so any
			// second inside it is already in the past.
			minute = rng.Intn(c.capMin)
		}
	}
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, second, 0, loc)
}

func writeSale(
	insertTxn, insertItem *sql.Stmt, rng *rand.Rand, loc *time.Location,
	day time.Time, clock dayClock, pool []poolItem, stats *saleStats,
) pastSale {
	when := clock.pick(rng, day, loc)

	lineCount := basketShape[rng.Intn(len(basketShape))]
	seen := make(map[int]bool, lineCount)

	var lines []saleLine
	total := 0
	for i := 0; i < lineCount; i++ {
		it := pool[rng.Intn(len(pool))]
		// One line per product per sale. checkoutHandler can write two lines
		// for the same product, but keeping it to one here means the
		// generated data reads the same way the return screen shows it.
		if seen[it.id] {
			continue
		}
		seen[it.id] = true

		qty := 1
		if it.price < 200 {
			qty = 1 + rng.Intn(3)
		} else if it.price < 800 && rng.Float64() < 0.3 {
			qty = 2
		}
		lines = append(lines, saleLine{productID: it.id, qty: qty, unitPrice: it.price, cost: it.cost})
		total += it.price * qty
	}
	if len(lines) == 0 {
		it := pool[rng.Intn(len(pool))]
		lines = append(lines, saleLine{productID: it.id, qty: 1, unitPrice: it.price, cost: it.cost})
		total = it.price
	}

	// Cash is still how most of this store gets paid.
	method := "cash"
	if rng.Float64() < 0.22 {
		method = "card"
	}

	res, err := insertTxn.Exec(total, method, "sale", nil, utcStamp(when))
	if err != nil {
		log.Fatalf("insert sale: %v", err)
	}
	id, _ := res.LastInsertId()

	for _, l := range lines {
		if _, err := insertItem.Exec(id, l.productID, l.qty, l.unitPrice, l.unitPrice*l.qty, l.cost); err != nil {
			log.Fatalf("insert sale item: %v", err)
		}
		stats.items++
	}

	stats.sales++
	stats.saleRupees += total
	return pastSale{id: id, when: when, method: method, lines: lines}
}

// writeReturn refunds part of an earlier sale. It follows the sign convention
// locked in decision 19: NEGATIVE quantity and NEGATIVE line total, POSITIVE
// per-unit price and cost, and the cost copied from the original sale line.
// The refund uses the payment method of the original sale, which is what the
// return screen preselects.
func writeReturn(
	insertTxn, insertItem *sql.Stmt, rng *rand.Rand, loc *time.Location,
	day time.Time, clock dayClock, orig pastSale, stats *saleStats,
) {
	when := clock.pick(rng, day, loc)

	// Most refunds are one line of the receipt, not the whole basket.
	line := orig.lines[rng.Intn(len(orig.lines))]
	qty := 1
	if line.qty > 1 && rng.Float64() < 0.35 {
		qty = 1 + rng.Intn(line.qty)
	}
	refund := line.unitPrice * qty

	res, err := insertTxn.Exec(-refund, orig.method, "return", orig.id, utcStamp(when))
	if err != nil {
		log.Fatalf("insert return: %v", err)
	}
	id, _ := res.LastInsertId()

	if _, err := insertItem.Exec(id, line.productID, -qty, line.unitPrice, -refund, line.cost); err != nil {
		log.Fatalf("insert return item: %v", err)
	}

	stats.returns++
	stats.items++
}

// generateReceiving writes finalized shipments so that the purchase report and
// the sale-versus-purchase comparison have rows. Like the sales above, these
// are historical records only. They do not move stock.
func generateReceiving(
	tx *sql.Tx, rng *rand.Rand, loc *time.Location,
	start, today time.Time, supplierIDs []int64, items []poolItem,
) int {
	if len(supplierIDs) == 0 || len(items) == 0 {
		return 0
	}

	insertSession, err := tx.Prepare(
		`INSERT INTO receiving_sessions (label, supplier_id, status, started_at, finalized_at)
		 VALUES (?, ?, 'finalized', ?, ?)`)
	if err != nil {
		log.Fatalf("prepare session insert: %v", err)
	}
	defer insertSession.Close()

	insertSessionItem, err := tx.Prepare(
		`INSERT OR IGNORE INTO receiving_session_items (session_id, product_id, qty) VALUES (?, ?, ?)`)
	if err != nil {
		log.Fatalf("prepare session item insert: %v", err)
	}
	defer insertSessionItem.Close()

	count := 0
	totalDays := int(today.Sub(start).Hours()/24) + 1
	for d := 0; d < totalDays; d++ {
		day := start.AddDate(0, 0, d)
		// A shipment arrives on most weekdays, not every day.
		if day.Weekday() == time.Sunday || rng.Float64() > 0.45 {
			continue
		}

		trucks := 1 + rng.Intn(2)
		for t := 0; t < trucks; t++ {
			started := time.Date(day.Year(), day.Month(), day.Day(),
				8+rng.Intn(4), rng.Intn(60), 0, 0, loc)
			finalized := started.Add(time.Duration(20+rng.Intn(90)) * time.Minute)
			// A shipment that has not finished arriving yet is not history.
			// This only ever trims the last day of the range.
			if finalized.After(time.Now().In(loc)) {
				continue
			}

			label := fmt.Sprintf("Truck %d — %s", t+1, day.Format("2 Jan"))
			res, err := insertSession.Exec(
				label, supplierIDs[rng.Intn(len(supplierIDs))],
				utcStamp(started), utcStamp(finalized))
			if err != nil {
				log.Fatalf("insert session: %v", err)
			}
			sessionID, _ := res.LastInsertId()

			// Tuned so that total purchase cost lands near 65 percent of total
			// sale revenue across the range, which is a believable grocery gross
			// margin. The first version bought 1.8 times what the store sold and
			// made the sale-versus-purchase panel look broken. Measure the two
			// column totals on the dashboard after you change these numbers.
			lineCount := 5 + rng.Intn(16)
			for i := 0; i < lineCount; i++ {
				it := items[rng.Intn(len(items))]
				qty := 3 + rng.Intn(22)
				if _, err := insertSessionItem.Exec(sessionID, it.id, qty); err != nil {
					log.Fatalf("insert session item: %v", err)
				}
			}
			count++
		}
	}
	return count
}

// utcStamp formats a store-local time the way SQLite's CURRENT_TIMESTAMP does:
// UTC, no zone suffix. Every report in main.go reads these rows back with
// date(created_at, '+5 hours'), so the stored value must be UTC or every row
// lands on the wrong day.
func utcStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
