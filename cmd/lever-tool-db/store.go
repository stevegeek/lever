package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// allowedTables is the reference tool's hard table allowlist. C is deliberately
// absent: it exists in the DB so the backstop can prove it is unreachable.
var allowedTables = map[string]bool{"A": true, "B": true}

// Store is the reference tool's SQLite-backed data store.
type Store struct{ db *sql.DB }

// OpenStore opens (pure-Go) SQLite at dsn and seeds tables A, B, C.
func OpenStore(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("ref: open: %w", err)
	}
	s := &Store{db: db}
	if err := s.seed(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) seed() error {
	for _, tbl := range []string{"A", "B", "C"} {
		if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS "` + tbl + `" (id INTEGER PRIMARY KEY, owner TEXT)`); err != nil {
			return fmt.Errorf("ref: create %s: %w", tbl, err)
		}
	}
	// seedData groups rows by table. Idempotency is enforced per-table: if the
	// table already has rows we skip it entirely, so reopening the same DB never
	// duplicates rows. Each owner value rides as a bound ? parameter (never
	// string-concatenated into the query).
	seedData := []struct {
		tbl  string
		rows []string
	}{
		{"A", []string{"alice", "alice", "bob"}},
		{"B", []string{"bob", "carol"}},
		{"C", []string{"secret", "secret"}},
	}
	for _, td := range seedData {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM "` + td.tbl + `"`).Scan(&n); err != nil {
			return fmt.Errorf("ref: count %s: %w", td.tbl, err)
		}
		if n > 0 {
			continue // table already seeded — skip to preserve idempotency
		}
		for _, owner := range td.rows {
			if _, err := s.db.Exec(`INSERT INTO "`+td.tbl+`" (owner) VALUES (?)`, owner); err != nil {
				return fmt.Errorf("ref: seed %s: %w", td.tbl, err)
			}
		}
	}
	return nil
}

// Read returns rows of <table> whose owner == filter. table must be in the
// allowlist (the table name cannot be a bound parameter, so the allowlist IS
// the injection guard); filter rides as a bound ? parameter (injection-inert).
func (s *Store) Read(table, filter string) ([]map[string]any, error) {
	if !allowedTables[table] {
		return nil, fmt.Errorf("ref: table %q not allowed", table)
	}
	rows, err := s.db.Query(`SELECT id, owner FROM "`+table+`" WHERE owner = ?`, filter)
	if err != nil {
		return nil, fmt.Errorf("ref: query: %w", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id int
		var owner string
		if err := rows.Scan(&id, &owner); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "owner": owner})
	}
	return out, rows.Err()
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }
