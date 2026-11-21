// Command purego benchmarks one SQLite driver on the shapes Offgrid cares about, so the
// cgo driver (mattn, the product's choice) and the pure-Go port (modernc) can be compared on the
// same machine, same schema, same pragmas:
//
//	go run .                          # mattn (cgo)
//	go run -tags modernc .            # modernc (pure Go; needs: go get modernc.org/sqlite && go mod tidy)
//
// Output is one line per measurement: "<driver> <name> <value> <unit>". Run both on the disk you
// care about (laptop SSD, the Pi's SD card). fsync cost dominates the write numbers.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func main() {
	n := flag.Int("n", 2000, "rows per write measurement")
	g := flag.Int("g", 16, "goroutines for the grouped measurement")
	dir := flag.String("dir", "", "directory for the database (default: a temp dir; choose the disk you care about)")
	flag.Parse()
	d := *dir
	if d == "" {
		var err error
		if d, err = os.MkdirTemp("", "purego"); err != nil {
			panic(err)
		}
		defer os.RemoveAll(d)
	}
	path := filepath.Join(d, "bench.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"
	if driverName == "sqlite3" {
		dsn = path + "?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000&_txlock=immediate"
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	must(db.Exec(`CREATE TABLE ops(id INTEGER PRIMARY KEY, node TEXT, seq INTEGER, payload BLOB, UNIQUE(node, seq))`))
	payload := make([]byte, 200)

	report := func(name string, v float64, unit string) { fmt.Printf("%s %s %.1f %s\n", driverName, name, v, unit) }

	// 1. one transaction per insert (what the store did before Day 46)
	ins, err := db.Prepare(`INSERT INTO ops(node, seq, payload) VALUES('a', ?, ?)`)
	if err != nil {
		panic(err)
	}
	t0 := time.Now()
	for i := 0; i < *n; i++ {
		tx, err := db.Begin()
		if err != nil {
			panic(err)
		}
		must(tx.Stmt(ins).Exec(i, payload))
		if err := tx.Commit(); err != nil {
			panic(err)
		}
	}
	report("insert_one_tx_each", float64(*n)/time.Since(t0).Seconds(), "rows/s")

	// 2. g goroutines each wanting an acknowledged write, sharing a commit (group commit)
	q := make(chan chan struct{}, 1024)
	type item struct {
		seq  int
		done chan struct{}
	}
	items := make(chan item, 1024)
	_ = q
	stop := make(chan struct{})
	var wgw sync.WaitGroup
	wgw.Add(1)
	go func() { // the single writer
		defer wgw.Done()
		for {
			var first item
			select {
			case first = <-items:
			case <-stop:
				return
			}
			batch := []item{first}
		more:
			for len(batch) < 128 {
				select {
				case it := <-items:
					batch = append(batch, it)
				default:
					break more
				}
			}
			tx, err := db.Begin()
			if err != nil {
				panic(err)
			}
			st := tx.Stmt(ins)
			for _, it := range batch {
				must(st.Exec(1_000_000+it.seq, payload))
			}
			if err := tx.Commit(); err != nil {
				panic(err)
			}
			for _, it := range batch {
				close(it.done)
			}
		}
	}()
	per := *n / *g
	t0 = time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *g; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				it := item{seq: w*per + i, done: make(chan struct{})}
				items <- it
				<-it.done
			}
		}(w)
	}
	wg.Wait()
	report(fmt.Sprintf("insert_group_%d_writers", *g), float64(per**g)/time.Since(t0).Seconds(), "rows/s")
	close(stop)
	wgw.Wait()

	// 3. point reads and a range scan (CPU-bound; the driver's own overhead shows here)
	sel, err := db.Prepare(`SELECT payload FROM ops WHERE node='a' AND seq=?`)
	if err != nil {
		panic(err)
	}
	const reads = 50000
	t0 = time.Now()
	for i := 0; i < reads; i++ {
		var b []byte
		if err := sel.QueryRow(i % *n).Scan(&b); err != nil {
			panic(err)
		}
	}
	report("point_read", float64(reads)/time.Since(t0).Seconds(), "reads/s")
	t0 = time.Now()
	for i := 0; i < 20; i++ {
		rows, err := db.Query(`SELECT id, node, seq, payload FROM ops`)
		if err != nil {
			panic(err)
		}
		c := 0
		for rows.Next() {
			var id, seq int
			var node string
			var b []byte
			if err := rows.Scan(&id, &node, &seq, &b); err != nil {
				panic(err)
			}
			c++
		}
		rows.Close()
		_ = c
	}
	report("full_scan_rows", float64(20*(*n+per**g))/time.Since(t0).Seconds(), "rows/s")
}

func must(_ sql.Result, err error) {
	if err != nil {
		panic(err)
	}
}
