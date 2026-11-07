// Package catalog stores table schemas and defines how rows are encoded,
// all on top of the storage engine.
//
// Key layout:
//
//	meta/<table>            -> JSON schema of the table
//	row/<table>/<rowid>     -> JSON object {"<column id>": value, ...}
//
// Every column has a permanent numeric ID, and rows store values by column ID.
// This makes ALTER TABLE cheap and atomic:
//   - ADD COLUMN: old rows simply have no value for the new ID (reads as NULL).
//   - DROP COLUMN: only the schema changes; stale values are ignored, and
//     re-adding a column with the same name gets a fresh ID, so old data
//     never comes back.
//
// The catalog is not safe for concurrent use by multiple goroutines; the
// shell runs one command at a time.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"godb/internal/storage"
)

// Type is a column type.
type Type string

const (
	TypeInt   Type = "INT"
	TypeText  Type = "TEXT"
	TypeFloat Type = "FLOAT"
	TypeBool  Type = "BOOL"
)

// ParseType converts a type name (any case) to a Type.
func ParseType(s string) (Type, bool) {
	switch Type(strings.ToUpper(s)) {
	case TypeInt:
		return TypeInt, true
	case TypeText:
		return TypeText, true
	case TypeFloat:
		return TypeFloat, true
	case TypeBool:
		return TypeBool, true
	}
	return "", false
}

var (
	ErrTableExists  = errors.New("table already exists")
	ErrNoTable      = errors.New("no such table")
	ErrColumnExists = errors.New("column already exists")
	ErrNoColumn     = errors.New("no such column")
	ErrLastColumn   = errors.New("cannot drop the only column of a table")
	ErrBadName      = errors.New("invalid name (use letters, digits and underscore; not starting with a digit)")
	ErrNoColumns    = errors.New("a table needs at least one column")
)

// Column is one column of a table.
type Column struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type Type   `json:"type"`
}

// Table is a table schema plus counters for new IDs.
type Table struct {
	Name      string   `json:"name"`
	Columns   []Column `json:"columns"`
	NextColID int      `json:"next_col_id"`
	NextRowID uint64   `json:"next_row_id"`
}

// ColumnIndex returns the position of the named column, or -1.
func (t *Table) ColumnIndex(name string) int {
	name = strings.ToLower(name)
	for i, c := range t.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------

// MetaKey is the engine key holding a table's schema.
func MetaKey(table string) string { return "meta/" + table }

// RowPrefix is the key prefix shared by all rows of a table.
func RowPrefix(table string) string { return "row/" + table + "/" }

// RowKey is the engine key of one row. The ID is zero-padded so that
// keys sort in insertion order.
func RowKey(table string, id uint64) string {
	return fmt.Sprintf("%s%020d", RowPrefix(table), id)
}

// ---------------------------------------------------------------------------
// Catalog
// ---------------------------------------------------------------------------

// Catalog manages table schemas inside a storage engine.
type Catalog struct {
	db *storage.Engine
}

// New returns a catalog backed by db.
func New(db *storage.Engine) *Catalog { return &Catalog{db: db} }

// Engine returns the underlying storage engine (the executor uses it to
// build batches that touch both rows and schema).
func (c *Catalog) Engine() *storage.Engine { return c.db }

func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		if r == '_' || unicode.IsLetter(r) {
			continue
		}
		if i > 0 && unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
}

// MarshalTable encodes a schema for storage under MetaKey.
func MarshalTable(t *Table) ([]byte, error) { return json.Marshal(t) }

// SaveTable writes the schema.
func (c *Catalog) SaveTable(t *Table) error {
	data, err := MarshalTable(t)
	if err != nil {
		return err
	}
	return c.db.Put(MetaKey(t.Name), data)
}

// GetTable loads a table schema.
func (c *Catalog) GetTable(name string) (*Table, error) {
	name = strings.ToLower(name)
	data, ok := c.db.Get(MetaKey(name))
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoTable, name)
	}
	var t Table
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("corrupt schema for table %s: %w", name, err)
	}
	return &t, nil
}

// ListTables returns all table names in sorted order.
func (c *Catalog) ListTables() []string {
	var names []string
	for _, kv := range c.db.Scan("meta/") {
		names = append(names, strings.TrimPrefix(kv.Key, "meta/"))
	}
	return names
}

// CreateTable creates a new table. Column IDs start at 1.
func (c *Catalog) CreateTable(name string, cols []Column) (*Table, error) {
	name = strings.ToLower(name)
	if !validName(name) {
		return nil, ErrBadName
	}
	if len(cols) == 0 {
		return nil, ErrNoColumns
	}
	if _, ok := c.db.Get(MetaKey(name)); ok {
		return nil, fmt.Errorf("%w: %s", ErrTableExists, name)
	}

	t := &Table{Name: name, NextColID: 1, NextRowID: 1}
	seen := map[string]bool{}
	for _, col := range cols {
		cn := strings.ToLower(col.Name)
		if !validName(cn) {
			return nil, fmt.Errorf("%w: %q", ErrBadName, col.Name)
		}
		if seen[cn] {
			return nil, fmt.Errorf("%w: %s", ErrColumnExists, cn)
		}
		seen[cn] = true
		t.Columns = append(t.Columns, Column{ID: t.NextColID, Name: cn, Type: col.Type})
		t.NextColID++
	}

	// A crash in the middle of a large DROP TABLE can leave orphan rows.
	// Clear them so a new table with the same name starts empty.
	if err := c.deleteKeys("", c.keysOf(RowPrefix(name))); err != nil {
		return nil, err
	}
	if err := c.SaveTable(t); err != nil {
		return nil, err
	}
	return t, nil
}

// DropTable removes the schema and all rows. The schema and the first
// batch of rows go in one atomic batch; any rows beyond the batch limit
// are removed afterwards (orphans are harmless and cleaned on re-create).
func (c *Catalog) DropTable(name string) error {
	name = strings.ToLower(name)
	if _, err := c.GetTable(name); err != nil {
		return err
	}
	return c.deleteKeys(MetaKey(name), c.keysOf(RowPrefix(name)))
}

// AddColumn appends a column. Existing rows read it as NULL.
func (c *Catalog) AddColumn(table, column string, typ Type) error {
	t, err := c.GetTable(table)
	if err != nil {
		return err
	}
	column = strings.ToLower(column)
	if !validName(column) {
		return fmt.Errorf("%w: %q", ErrBadName, column)
	}
	if t.ColumnIndex(column) >= 0 {
		return fmt.Errorf("%w: %s", ErrColumnExists, column)
	}
	t.Columns = append(t.Columns, Column{ID: t.NextColID, Name: column, Type: typ})
	t.NextColID++
	return c.SaveTable(t)
}

// DropColumn removes a column from the schema (one atomic write).
func (c *Catalog) DropColumn(table, column string) error {
	t, err := c.GetTable(table)
	if err != nil {
		return err
	}
	idx := t.ColumnIndex(column)
	if idx < 0 {
		return fmt.Errorf("%w: %s", ErrNoColumn, strings.ToLower(column))
	}
	if len(t.Columns) == 1 {
		return ErrLastColumn
	}
	t.Columns = append(t.Columns[:idx], t.Columns[idx+1:]...)
	return c.SaveTable(t)
}

func (c *Catalog) keysOf(prefix string) []string {
	kvs := c.db.Scan(prefix)
	keys := make([]string, len(kvs))
	for i, kv := range kvs {
		keys[i] = kv.Key
	}
	return keys
}

// deleteKeys deletes first (if non-empty) and keys, in batches that respect
// the engine's batch size limit. first goes in the first batch.
func (c *Catalog) deleteKeys(first string, keys []string) error {
	if first == "" && len(keys) == 0 {
		return nil
	}
	b := c.db.NewBatch()
	n := 0
	if first != "" {
		b.Delete(first)
		n++
	}
	for _, k := range keys {
		if n == storage.MaxBatchOps {
			if err := b.Commit(); err != nil {
				return err
			}
			b = c.db.NewBatch()
			n = 0
		}
		b.Delete(k)
		n++
	}
	return b.Commit()
}

// ---------------------------------------------------------------------------
// Values and row encoding
// ---------------------------------------------------------------------------
//
// Go representation of values: int64, float64, string, bool, or nil (NULL).

// Coerce converts v to the Go type for typ, or returns an error.
// NULL (nil) is accepted for every type; INT is accepted for FLOAT.
func Coerce(v any, typ Type) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch typ {
	case TypeInt:
		if x, ok := v.(int64); ok {
			return x, nil
		}
	case TypeFloat:
		switch x := v.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		}
	case TypeText:
		if x, ok := v.(string); ok {
			return x, nil
		}
	case TypeBool:
		if x, ok := v.(bool); ok {
			return x, nil
		}
	}
	return nil, fmt.Errorf("cannot use %s value %v as %s", describe(v), v, typ)
}

func describe(v any) string {
	switch v.(type) {
	case int64:
		return "INT"
	case float64:
		return "FLOAT"
	case string:
		return "TEXT"
	case bool:
		return "BOOL"
	}
	return "unknown"
}

// FormatValue renders a value for display.
func FormatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return x
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	}
	return fmt.Sprint(v)
}

// EncodeRow encodes vals (one per column, in schema order, already coerced)
// as a JSON object keyed by column ID. NULLs are omitted.
func EncodeRow(t *Table, vals []any) ([]byte, error) {
	if len(vals) != len(t.Columns) {
		return nil, fmt.Errorf("expected %d values, got %d", len(t.Columns), len(vals))
	}
	obj := make(map[string]any, len(vals))
	for i, col := range t.Columns {
		if vals[i] != nil {
			obj[strconv.Itoa(col.ID)] = vals[i]
		}
	}
	return json.Marshal(obj)
}

// DecodeRow decodes a stored row into values in current schema order.
// Missing values (added column, NULL) come back as nil; values of dropped
// columns are ignored.
func DecodeRow(t *Table, data []byte) ([]any, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("corrupt row: %w", err)
	}
	vals := make([]any, len(t.Columns))
	for i, col := range t.Columns {
		r, ok := raw[strconv.Itoa(col.ID)]
		if !ok || string(r) == "null" {
			continue
		}
		var err error
		switch col.Type {
		case TypeInt:
			var x int64
			err = json.Unmarshal(r, &x)
			vals[i] = x
		case TypeFloat:
			var x float64
			err = json.Unmarshal(r, &x)
			vals[i] = x
		case TypeText:
			var x string
			err = json.Unmarshal(r, &x)
			vals[i] = x
		case TypeBool:
			var x bool
			err = json.Unmarshal(r, &x)
			vals[i] = x
		}
		if err != nil {
			return nil, fmt.Errorf("corrupt value for column %s: %w", col.Name, err)
		}
	}
	return vals, nil
}
