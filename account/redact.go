package account

import (
	"context"
	"database/sql"
	"regexp"
)

// SecretColumns matches column and field names whose values an export must
// never carry: password and token hashes, keys, sealed secrets, 2FA material.
var SecretColumns = regexp.MustCompile(`(?i)pass|secret|token|hash|cipher|encrypt|_iv$|nonce|totp|recovery|priv(ate)?_?key|api_?key|credential`)

// Redacted replaces hidden values unless a Redactor names another mask.
const Redacted = "[redacted]"

// Redactor hides secret values in exported rows and maps.
type Redactor struct {
	Columns *regexp.Regexp // names to hide; SecretColumns when nil
	Keep    []string       // names never hidden, such as "password_login_enabled"
	Mask    any            // replacement; Redacted when nil
}

// Hidden reports whether a name's value is withheld.
func (r *Redactor) Hidden(name string) bool {
	for _, k := range r.Keep {
		if k == name {
			return false
		}
	}
	re := r.Columns
	if re == nil {
		re = SecretColumns
	}
	return re.MatchString(name)
}

// Value returns v, or the mask when the name is hidden. Absent values stay
// absent.
func (r *Redactor) Value(name string, v any) any {
	if v == nil || !r.Hidden(name) {
		return v
	}
	if r.Mask != nil {
		return r.Mask
	}
	return Redacted
}

// Map redacts a decoded JSON value in place, descending into nested objects
// and arrays, and returns it.
func (r *Redactor) Map(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if r.Hidden(k) {
				t[k] = r.Value(k, x)
			} else {
				t[k] = r.Map(x)
			}
		}
	case []any:
		for i, x := range t {
			t[i] = r.Map(x)
		}
	}
	return v
}

// RowsQuerier queries many rows; *sql.DB and *sql.Tx satisfy it.
type RowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// TablesWithColumn lists the tables that have a column, such as every table
// keyed by user_id, for exports and erasure that follow the schema.
func TablesWithColumn(ctx context.Context, q RowsQuerier, column string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT m.name FROM sqlite_master m, pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND p.name = ? ORDER BY m.name`, column)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
