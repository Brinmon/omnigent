// Package store holds the generated queries and the schema they target.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
)

// schemaSQL is the table definitions, exported from a Python-created database so
// the two builds share one on-disk shape.
//
//go:embed migrations/0001_python_schema.sql
var schemaSQL string

// EnsureSchema creates the tables a fresh database needs.
//
// Without this, `serve --database-uri` against a new file starts successfully and
// then fails every query with "no such table", which reads as a broken build
// rather than an uninitialised database. The statements are CREATE TABLE IF NOT
// EXISTS, so an existing database is untouched and this is safe on every start.
//
// It does not migrate: a schema written by a newer Python build is used as-is, and
// a mismatch surfaces as a query error naming the column rather than as a silent
// rewrite of the user's data.
func EnsureSchema(conn *sql.DB) error {
	statements := splitStatements(schemaSQL)
	if len(statements) == 0 {
		return fmt.Errorf("the embedded schema is empty")
	}
	for _, statement := range statements {
		if _, err := conn.Exec(makeIdempotent(statement)); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}
	return nil
}

// makeIdempotent rewrites CREATE TABLE so a second start succeeds.
//
// The embedded file is exported verbatim from the Python database, which has no
// IF NOT EXISTS because it is applied by a migrator that tracks its own progress.
// This build applies it on every start instead, so the guard is added here rather
// than by editing the export -- keeping the file a faithful record of the shape
// the two builds share.
func makeIdempotent(statement string) string {
	// Statements carry a leading "-- table_name" comment, so the CREATE keyword is
	// found rather than assumed to be first.
	const marker = "CREATE TABLE "
	index := strings.Index(statement, marker)
	if index < 0 {
		return statement
	}
	after := statement[index+len(marker):]
	if strings.HasPrefix(after, "IF NOT EXISTS") {
		return statement
	}
	return statement[:index] + marker + "IF NOT EXISTS " + after
}

// splitStatements breaks the schema into individual statements.
//
// SQLite's driver refuses a multi-statement Exec, so they are split here. The file
// holds only CREATE TABLE and CREATE INDEX statements with no semicolons inside
// string literals, so splitting on the terminator is sound.
func splitStatements(sqlText string) []string {
	var statements []string
	for _, chunk := range strings.Split(sqlText, ";") {
		trimmed := strings.TrimSpace(chunk)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") && !strings.Contains(trimmed, "\n") {
			continue
		}
		statements = append(statements, trimmed)
	}
	return statements
}
