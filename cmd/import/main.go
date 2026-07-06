package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "pos.db", "path to SQLite database")
	csvPath := flag.String("csv", "oldPOSdata/products_export.csv", "path to BCP CSV export")
	flag.Parse()

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	f, err := os.Open(*csvPath)
	if err != nil {
		log.Fatalf("open csv: %v", err)
	}
	defer f.Close()

	var inserted, skipped, dups int
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimRight(scanner.Text(), "\r\n")
		parts := strings.SplitN(line, ",", 6)
		if len(parts) != 6 {
			fmt.Printf("skip line %d (want 6 fields, got %d): %q\n", lineNum, len(parts), line)
			skipped++
			continue
		}

		barcode := normalizeBarcode(parts[0])
		name := strings.TrimSpace(parts[1])
		priceStr := strings.TrimSpace(parts[2])
		stockStr := strings.TrimSpace(parts[3])
		costStr := strings.TrimSpace(parts[4])
		category := strings.TrimSpace(parts[5])

		if barcode == "" || name == "" {
			fmt.Printf("skip line %d (empty barcode or name): %q\n", lineNum, line)
			skipped++
			continue
		}

		price, err := strconv.Atoi(priceStr)
		if err != nil || price <= 0 {
			fmt.Printf("skip line %d (bad price %q): %q\n", lineNum, priceStr, line)
			skipped++
			continue
		}

		stock, _ := strconv.Atoi(stockStr)
		cost, _ := strconv.Atoi(costStr)

		var costVal interface{}
		if cost > 0 {
			costVal = cost
		}
		var catVal interface{}
		if category != "" {
			catVal = category
		}

		res, err := db.Exec(
			`INSERT OR IGNORE INTO products (barcode, name, price_rupees, stock, cost_price_rupees, category) VALUES (?, ?, ?, ?, ?, ?)`,
			barcode, name, price, stock, costVal, catVal,
		)
		if err != nil {
			log.Fatalf("insert line %d: %v", lineNum, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			dups++
		} else {
			inserted++
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("scan: %v", err)
	}

	fmt.Printf("\nDone. inserted=%d  duplicates_skipped=%d  bad_rows=%d  total_lines=%d\n",
		inserted, dups, skipped, lineNum)
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
