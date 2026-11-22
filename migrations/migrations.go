package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// Files contains the versioned Platform MySQL migrations.
//
//go:embed *.sql
var Files embed.FS

// Names returns migration filenames in lexical application order.
func Names() ([]string, error) {
	names, err := fs.Glob(Files, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(names)
	return names, nil
}

// Apply executes all embedded migrations against an empty or correctly versioned database.
// MySQL DDL auto-commits, so callers must stop on the first returned error.
func Apply(ctx context.Context, db *sql.DB) error {
	names, err := Names()
	if err != nil {
		return err
	}
	for _, name := range names {
		script, err := Files.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		statements, err := SplitStatements(string(script))
		if err != nil {
			return fmt.Errorf("parse migration %s: %w", name, err)
		}
		for index, statement := range statements {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply migration %s statement %d: %w", name, index+1, err)
			}
		}
	}
	return nil
}

// SplitStatements splits trusted migration SQL without breaking quoted semicolons.
// Stored programs and DELIMITER directives are intentionally unsupported.
func SplitStatements(script string) ([]string, error) {
	var statements []string
	var current strings.Builder
	var quote byte
	lineComment := false
	blockComment := false

	for index := 0; index < len(script); index++ {
		char := script[index]
		next := byte(0)
		if index+1 < len(script) {
			next = script[index+1]
		}

		if lineComment {
			if char == '\n' || char == '\r' {
				lineComment = false
				current.WriteByte(' ')
			}
			continue
		}
		if blockComment {
			if char == '*' && next == '/' {
				blockComment = false
				index++
				current.WriteByte(' ')
			}
			continue
		}

		if quote != 0 {
			current.WriteByte(char)
			if char == '\\' && quote != '`' && next != 0 {
				current.WriteByte(next)
				index++
				continue
			}
			if char == quote {
				if next == quote {
					current.WriteByte(next)
					index++
					continue
				}
				quote = 0
			}
			continue
		}

		switch {
		case char == '-' && next == '-' && (index+2 == len(script) || isSpace(script[index+2])):
			lineComment = true
			index++
		case char == '#':
			lineComment = true
		case char == '/' && next == '*':
			blockComment = true
			index++
		case char == '\'' || char == '"' || char == '`':
			quote = char
			current.WriteByte(char)
		case char == ';':
			appendStatement(&statements, &current)
		default:
			current.WriteByte(char)
		}
	}

	if quote != 0 {
		return nil, fmt.Errorf("unterminated %q quote", quote)
	}
	if blockComment {
		return nil, fmt.Errorf("unterminated block comment")
	}
	appendStatement(&statements, &current)
	return statements, nil
}

func appendStatement(statements *[]string, current *strings.Builder) {
	statement := strings.TrimSpace(current.String())
	if statement != "" {
		*statements = append(*statements, statement)
	}
	current.Reset()
}

func isSpace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\n' || char == '\r' || char == '\f'
}
