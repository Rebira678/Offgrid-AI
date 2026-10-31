//go:build ignore

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"offgrid/internal/store/sqlstore"
	"offgrid/internal/task"
)

func prio(p task.Priority) *task.Priority { return &p }
func stat(s task.Status) *task.Status     { return &s }

func main() {
	dbPath := "./my-tasks.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	ctx := context.Background()
	st, err := sqlstore.Open(ctx, dbPath, sqlstore.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening db %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	defer st.Close()

	tasks, err := st.List(ctx, task.Filter{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error listing tasks: %v\n", err)
		os.Exit(1)
	}

	findTask := func(substr string) *task.Task {
		for _, t := range tasks {
			if strings.Contains(strings.ToLower(t.Title), strings.ToLower(substr)) {
				return &t
			}
		}
		return nil
	}

	// 1. T3: Clean solar panels
	tSolar := findTask("Clean solar panels")
	if tSolar != nil {
		sug, created, err := st.Suggest(ctx, task.Draft{
			Task:     tSolar.ID,
			Status:   stat(task.StatusDone),
			Priority: prio(task.PriorityLow),
			Note:     "Solar output back up to 98% after wash. Power steady, ready to mark done.",
			By:       "llama3.2:3b",
			Key:      "prop-solar-clean",
		})
		if err != nil {
			fmt.Printf("solar suggest err: %v\n", err)
		} else {
			fmt.Printf("Proposal 1 created: %s (created=%v)\n", sug.ID, created)
		}
	} else {
		fmt.Println("solar task not found")
	}

	// 2. T4: Replace water filter
	tFilter := findTask("Replace water filter")
	if tFilter != nil {
		sug, created, err := st.Suggest(ctx, task.Draft{
			Task:     tFilter.ID,
			Status:   stat(task.StatusInProgress),
			Note:     "Spare filter cartridge arrived at Site 2 gate. Ready to begin replacement.",
			By:       "llama3.2:3b",
			Key:      "prop-filter-replace",
		})
		if err != nil {
			fmt.Printf("filter suggest err: %v\n", err)
		} else {
			fmt.Printf("Proposal 2 created: %s (created=%v)\n", sug.ID, created)
		}
	} else {
		fmt.Println("filter task not found")
	}

	// 3. T10: Inspect fuel tank level
	tFuel := findTask("Inspect fuel tank")
	if tFuel != nil {
		sug, created, err := st.Suggest(ctx, task.Draft{
			Task:     tFuel.ID,
			Priority: prio(task.PriorityHigh),
			Note:     "Fuel tank measured at 35% with freeze expected tonight. Recommend scheduling refill.",
			By:       "llama3.2:3b",
			Key:      "prop-fuel-refill",
		})
		if err != nil {
			fmt.Printf("fuel suggest err: %v\n", err)
		} else {
			fmt.Printf("Proposal 3 created: %s (created=%v)\n", sug.ID, created)
		}
	} else {
		fmt.Println("fuel task not found")
	}

	// 4. T6: Order spare parts for pump (Remote Conflict / Overlap Demo)
	tSpare := findTask("Order spare parts")
	if tSpare != nil {
		sug, created, err := st.Suggest(ctx, task.Draft{
			Task:     tSpare.ID,
			Status:   stat(task.StatusInProgress),
			Priority: prio(task.PriorityHigh),
			Note:     "Supplier emailed tracking number. Seals shipped via priority courier.",
			By:       "llama3.2:3b",
			Key:      "prop-spare-conflict",
		})
		if err != nil {
			fmt.Printf("spare conflict suggest err: %v\n", err)
		} else {
			fmt.Printf("Proposal 4 (Conflict demo) created: %s (created=%v)\n", sug.ID, created)
		}
	} else {
		fmt.Println("spare task not found")
	}

	fmt.Println("Done creating AI copilot proposals.")
}
