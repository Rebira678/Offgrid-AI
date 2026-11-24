package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"offgrid/internal/growth"
	"offgrid/internal/store/sqlstore"
)

// growthCmd is "offgrid store -db F growth" (Day 49): measure, project a crew-year, and alarm.
//
//	offgrid store -db F growth                                  print the numbers and a one-year projection
//	offgrid store -db F growth -record                          also remember this sample (run it daily from cron)
//	offgrid store -db F growth -max-ops 500000 -max-db 1024 -max-blobs 4096 ...  exit 3 and say why when a line is crossed
//
// Sizes are in MiB. The history lives in <db>.growth.json and only the rate alarm needs it.
func growthCmd(ctx context.Context, st *sqlstore.Store, dbPath string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("growth", flag.ContinueOnError)
	fs.SetOutput(stderr)
	record := fs.Bool("record", false, "remember this sample in the history file")
	asJSON := fs.Bool("json", false, "print JSON")
	maxOps := fs.Int64("max-ops", 0, "alarm when the log passes this many ops (ADR 010 says 500000)")
	maxDB := fs.Int64("max-db", 0, "alarm when the database passes this many MiB")
	maxBlobs := fs.Int64("max-blobs", 0, "alarm when the blob folder passes this many MiB")
	dbDay := fs.Float64("max-db-per-day", 0, "alarm when the database grows faster than this many MiB a day")
	blobDay := fs.Float64("max-blobs-per-day", 0, "alarm when blobs grow faster than this many MiB a day")
	yearDB := fs.Int64("year-db", 0, "alarm when the database is projected past this many MiB in a year")
	yearBlobs := fs.Int64("year-blobs", 0, "alarm when the blob folder is projected past this many MiB in a year")
	window := fs.Duration("window", 7*24*time.Hour, "how far back the rate alarm looks")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	now := time.Now().UTC()
	cur, err := st.Growth(ctx, now)
	if err != nil {
		return storeError(stderr, err)
	}
	hist := growth.History{Path: dbPath + ".growth.json"}
	old, err := hist.Load()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	const m = 1 << 20
	alarms := growth.Check(cur, old, growth.Limits{
		MaxOps: *maxOps, MaxDB: *maxDB * m, MaxBlobs: *maxBlobs * m,
		MaxDBPerDay: *dbDay * m, MaxBlobPerDay: *blobDay * m,
		YearDB: *yearDB * m, YearBl: *yearBlobs * m, RateWindow: *window,
	})
	cal, act := growth.Project(cur, true), growth.Project(cur, false)
	if *record {
		if err := hist.Add(cur); err != nil {
			fmt.Fprintln(stderr, "error: cannot save the history:", err)
			return 1
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", " ")
		_ = enc.Encode(map[string]any{"sample": cur, "calendar": cal, "active_days": act, "alarms": alarms})
	} else {
		printGrowth(stdout, cur, cal, act, alarms)
	}
	if len(alarms) > 0 {
		return 3
	}
	return 0
}

func printGrowth(w io.Writer, s growth.Sample, cal, act growth.Projection, alarms []growth.Alarm) {
	mib := func(n int64) string { return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20)) }
	fmt.Fprintf(w, "ops in the log     %d (payload %s)\n", s.Ops, mib(s.OpBytes))
	fmt.Fprintf(w, "database file      %s\n", mib(s.DBBytes))
	fmt.Fprintf(w, "blob folder        %s in %d files\n", mib(s.BlobBytes), s.Blobs)
	if s.First.IsZero() {
		fmt.Fprintln(w, "history            the log is empty: nothing to project yet")
	} else {
		fmt.Fprintf(w, "history            %s to %s, %d days with activity\n", s.First.Format("2006-01-02"), s.Last.Format("2006-01-02"), s.ActiveDays)
		if s.Ops > 0 {
			fmt.Fprintf(w, "cost per op        %.0f bytes of database (a new database also holds about 0.1 MiB of fixed schema)\n", float64(s.DBBytes)/float64(s.Ops))
		}
		if !cal.Enough {
			fmt.Fprintln(w, "projection         withheld: the log spans under 7 days, and a short burst would be extrapolated into nonsense.\n                   Use the cost per op times your ops per day, or run again next week.")
		} else {
			for _, p := range []growth.Projection{cal, act} {
				fmt.Fprintf(w, "one year (%s, %.1f days): +%d ops, database +%s (to %s), blobs +%s (to %s)\n",
					p.Basis, p.DaysUsed, p.YearOps, mib(p.YearDB), mib(p.TotalDB), mib(p.YearBlobs), mib(p.TotalBlobs))
			}
		}
	}
	for _, a := range alarms {
		fmt.Fprintln(w, "ALARM              "+a.String())
	}
	if len(alarms) == 0 {
		fmt.Fprintln(w, "alarms             none")
	}
}
