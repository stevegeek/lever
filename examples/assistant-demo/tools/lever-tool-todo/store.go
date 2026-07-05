package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"strings"
)

// Todo is one row of the CSV todo list.
type Todo struct {
	ID       string `json:"id"`
	Task     string `json:"task"`
	Due      string `json:"due"`
	Priority string `json:"priority"`
	Done     bool   `json:"done"`
}

// Store reads todos from a CSV file on demand. Read-only, no external services —
// the point of the demo tool is to show a first-party capability tool end to
// end, not to be a real task manager.
type Store struct{ path string }

func OpenStore(path string) *Store { return &Store{path: path} }

// List returns all todos, optionally filtered to only the not-done ones.
// The CSV header is: id,task,due,priority,done
func (s *Store) List(pendingOnly bool) ([]Todo, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("open todos: %w", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse todos csv: %w", err)
	}
	var out []Todo
	for i, r := range rows {
		if i == 0 || len(r) < 5 { // skip header + short rows
			continue
		}
		done := strings.EqualFold(strings.TrimSpace(r[4]), "true")
		if pendingOnly && done {
			continue
		}
		out = append(out, Todo{
			ID: r[0], Task: r[1], Due: r[2], Priority: r[3], Done: done,
		})
	}
	return out, nil
}
