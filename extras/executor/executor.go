// Package executor runs parsed commands against the catalog and storage engine.
package executor

import (
	"fmt"

	"godb/extras/catalog"
	"godb/extras/sql"
	"godb/internal/storage"
)

// Result is what a command produces: either a table of rows (SELECT,
// SHOW TABLES, DESCRIBE) or just a message.
type Result struct {
	Columns []string
	Rows    [][]any // values are int64, float64, string, bool or nil
	Message string
}

// Executor runs commands.
type Executor struct {
	cat *catalog.Catalog
	db  *storage.Engine
}

// New returns an executor using the given catalog.
func New(cat *catalog.Catalog) *Executor {
	return &Executor{cat: cat, db: cat.Engine()}
}

// Execute runs one parsed command. HELP and EXIT belong to the shell.
func (x *Executor) Execute(cmd sql.Command) (*Result, error) {
	switch c := cmd.(type) {
	case sql.CreateTable:
		return x.createTable(c)
	case sql.DropTable:
		if err := x.cat.DropTable(c.Name); err != nil {
			return nil, err
		}
		return msg("Table %s dropped.", c.Name), nil
	case sql.AlterAddColumn:
		typ, ok := catalog.ParseType(c.Column.Type)
		if !ok {
			return nil, fmt.Errorf("unknown type %s", c.Column.Type)
		}
		if err := x.cat.AddColumn(c.Table, c.Column.Name, typ); err != nil {
			return nil, err
		}
		return msg("Column %s added to %s.", c.Column.Name, c.Table), nil
	case sql.AlterDropColumn:
		if err := x.cat.DropColumn(c.Table, c.Column); err != nil {
			return nil, err
		}
		return msg("Column %s dropped from %s.", c.Column, c.Table), nil
	case sql.ShowTables:
		return x.showTables(), nil
	case sql.Describe:
		return x.describe(c)
	case sql.Insert:
		return x.insert(c)
	case sql.Select:
		return x.selectRows(c)
	case sql.Update:
		return x.update(c)
	case sql.Delete:
		return x.delete(c)
	}
	return nil, fmt.Errorf("command not supported by the executor: %T", cmd)
}

func msg(format string, args ...any) *Result {
	return &Result{Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// Tables
// ---------------------------------------------------------------------------

func (x *Executor) createTable(c sql.CreateTable) (*Result, error) {
	cols := make([]catalog.Column, len(c.Columns))
	for i, d := range c.Columns {
		typ, ok := catalog.ParseType(d.Type)
		if !ok {
			return nil, fmt.Errorf("unknown type %s", d.Type)
		}
		cols[i] = catalog.Column{Name: d.Name, Type: typ}
	}
	if _, err := x.cat.CreateTable(c.Name, cols); err != nil {
		return nil, err
	}
	return msg("Table %s created.", c.Name), nil
}

func (x *Executor) showTables() *Result {
	res := &Result{Columns: []string{"table"}}
	for _, name := range x.cat.ListTables() {
		res.Rows = append(res.Rows, []any{name})
	}
	if len(res.Rows) == 0 {
		return &Result{Message: "No tables."}
	}
	return res
}

func (x *Executor) describe(c sql.Describe) (*Result, error) {
	t, err := x.cat.GetTable(c.Table)
	if err != nil {
		return nil, err
	}
	res := &Result{Columns: []string{"column", "type"}}
	for _, col := range t.Columns {
		res.Rows = append(res.Rows, []any{col.Name, string(col.Type)})
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

type storedRow struct {
	key  string
	vals []any
}

func (x *Executor) loadRows(t *catalog.Table) ([]storedRow, error) {
	kvs := x.db.Scan(catalog.RowPrefix(t.Name))
	rows := make([]storedRow, 0, len(kvs))
	for _, kv := range kvs {
		vals, err := catalog.DecodeRow(t, kv.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kv.Key, err)
		}
		rows = append(rows, storedRow{key: kv.Key, vals: vals})
	}
	return rows, nil
}

func (x *Executor) insert(c sql.Insert) (*Result, error) {
	t, err := x.cat.GetTable(c.Table)
	if err != nil {
		return nil, err
	}

	// pos[i] = table column that the i-th value of each row goes to.
	var pos []int
	if c.Columns == nil {
		pos = make([]int, len(t.Columns))
		for i := range pos {
			pos[i] = i
		}
	} else {
		seen := map[int]bool{}
		for _, name := range c.Columns {
			idx := t.ColumnIndex(name)
			if idx < 0 {
				return nil, fmt.Errorf("%w: %s", catalog.ErrNoColumn, name)
			}
			if seen[idx] {
				return nil, fmt.Errorf("column %s listed twice", name)
			}
			seen[idx] = true
			pos = append(pos, idx)
		}
	}

	if len(c.Rows)+1 > storage.MaxBatchOps {
		return nil, fmt.Errorf("too many rows in one INSERT (max %d)", storage.MaxBatchOps-1)
	}

	b := x.db.NewBatch()
	for n, row := range c.Rows {
		if len(row) != len(pos) {
			return nil, fmt.Errorf("row %d has %d values, expected %d", n+1, len(row), len(pos))
		}
		vals := make([]any, len(t.Columns))
		for i, v := range row {
			col := t.Columns[pos[i]]
			cv, err := catalog.Coerce(v, col.Type)
			if err != nil {
				return nil, fmt.Errorf("column %s: %w", col.Name, err)
			}
			vals[pos[i]] = cv
		}
		data, err := catalog.EncodeRow(t, vals)
		if err != nil {
			return nil, err
		}
		b.Put(catalog.RowKey(t.Name, t.NextRowID), data)
		t.NextRowID++
	}

	// Rows and the updated row counter commit together.
	meta, err := catalog.MarshalTable(t)
	if err != nil {
		return nil, err
	}
	b.Put(catalog.MetaKey(t.Name), meta)
	if err := b.Commit(); err != nil {
		return nil, err
	}
	return msg("%d row(s) inserted.", len(c.Rows)), nil
}

func (x *Executor) selectRows(c sql.Select) (*Result, error) {
	t, err := x.cat.GetTable(c.Table)
	if err != nil {
		return nil, err
	}
	pred, err := compileWhere(t, c.Where)
	if err != nil {
		return nil, err
	}

	// Which columns to output.
	var idxs []int
	if c.Columns == nil {
		for i := range t.Columns {
			idxs = append(idxs, i)
		}
	} else {
		for _, name := range c.Columns {
			idx := t.ColumnIndex(name)
			if idx < 0 {
				return nil, fmt.Errorf("%w: %s", catalog.ErrNoColumn, name)
			}
			idxs = append(idxs, idx)
		}
	}

	rows, err := x.loadRows(t)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	for _, idx := range idxs {
		res.Columns = append(res.Columns, t.Columns[idx].Name)
	}
	for _, r := range rows {
		if !pred(r.vals) {
			continue
		}
		out := make([]any, len(idxs))
		for i, idx := range idxs {
			out[i] = r.vals[idx]
		}
		res.Rows = append(res.Rows, out)
	}
	return res, nil
}

func (x *Executor) update(c sql.Update) (*Result, error) {
	t, err := x.cat.GetTable(c.Table)
	if err != nil {
		return nil, err
	}
	pred, err := compileWhere(t, c.Where)
	if err != nil {
		return nil, err
	}

	type set struct {
		idx int
		val any
	}
	var sets []set
	seen := map[int]bool{}
	for _, a := range c.Sets {
		idx := t.ColumnIndex(a.Column)
		if idx < 0 {
			return nil, fmt.Errorf("%w: %s", catalog.ErrNoColumn, a.Column)
		}
		if seen[idx] {
			return nil, fmt.Errorf("column %s is set twice", a.Column)
		}
		seen[idx] = true
		v, err := catalog.Coerce(a.Value, t.Columns[idx].Type)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", a.Column, err)
		}
		sets = append(sets, set{idx, v})
	}

	rows, err := x.loadRows(t)
	if err != nil {
		return nil, err
	}

	// Build the whole batch first; nothing is written unless all rows are fine.
	b := x.db.NewBatch()
	n := 0
	for _, r := range rows {
		if !pred(r.vals) {
			continue
		}
		for _, s := range sets {
			r.vals[s.idx] = s.val
		}
		data, err := catalog.EncodeRow(t, r.vals)
		if err != nil {
			return nil, err
		}
		b.Put(r.key, data)
		n++
		if n > storage.MaxBatchOps {
			return nil, fmt.Errorf("too many rows in one UPDATE (max %d); narrow the WHERE", storage.MaxBatchOps)
		}
	}
	if err := b.Commit(); err != nil {
		return nil, err
	}
	return msg("%d row(s) updated.", n), nil
}

func (x *Executor) delete(c sql.Delete) (*Result, error) {
	t, err := x.cat.GetTable(c.Table)
	if err != nil {
		return nil, err
	}
	pred, err := compileWhere(t, c.Where)
	if err != nil {
		return nil, err
	}
	rows, err := x.loadRows(t)
	if err != nil {
		return nil, err
	}

	b := x.db.NewBatch()
	n := 0
	for _, r := range rows {
		if !pred(r.vals) {
			continue
		}
		b.Delete(r.key)
		n++
		if n > storage.MaxBatchOps {
			return nil, fmt.Errorf("too many rows in one DELETE (max %d); narrow the WHERE or use DROP TABLE", storage.MaxBatchOps)
		}
	}
	if err := b.Commit(); err != nil {
		return nil, err
	}
	return msg("%d row(s) deleted.", n), nil
}

// ---------------------------------------------------------------------------
// WHERE evaluation
// ---------------------------------------------------------------------------

type predicate func(vals []any) bool

// compileWhere resolves column names and literal types once, up front, so
// errors appear before any work is done. A nil expression matches every row.
func compileWhere(t *catalog.Table, e sql.Expr) (predicate, error) {
	if e == nil {
		return func([]any) bool { return true }, nil
	}
	switch x := e.(type) {
	case sql.Logical:
		left, err := compileWhere(t, x.Left)
		if err != nil {
			return nil, err
		}
		right, err := compileWhere(t, x.Right)
		if err != nil {
			return nil, err
		}
		if x.Op == "AND" {
			return func(v []any) bool { return left(v) && right(v) }, nil
		}
		return func(v []any) bool { return left(v) || right(v) }, nil

	case sql.Comparison:
		idx := t.ColumnIndex(x.Column)
		if idx < 0 {
			return nil, fmt.Errorf("%w: %s", catalog.ErrNoColumn, x.Column)
		}
		switch x.Op {
		case "IS NULL":
			return func(v []any) bool { return v[idx] == nil }, nil
		case "IS NOT NULL":
			return func(v []any) bool { return v[idx] != nil }, nil
		}
		if x.Value == nil {
			return nil, fmt.Errorf("cannot compare with NULL using %s; use IS NULL or IS NOT NULL", x.Op)
		}
		lit, err := catalog.Coerce(x.Value, t.Columns[idx].Type)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", x.Column, err)
		}
		op := x.Op
		return func(v []any) bool {
			if v[idx] == nil {
				return false // NULL never matches a comparison
			}
			return applyOp(op, compareValues(v[idx], lit))
		}, nil
	}
	return nil, fmt.Errorf("unsupported WHERE expression %T", e)
}

func applyOp(op string, cmp int) bool {
	switch op {
	case "=":
		return cmp == 0
	case "!=":
		return cmp != 0
	case "<":
		return cmp < 0
	case ">":
		return cmp > 0
	case "<=":
		return cmp <= 0
	case ">=":
		return cmp >= 0
	}
	return false
}

// compareValues returns -1, 0 or 1. Both values have the column's type.
func compareValues(a, b any) int {
	switch x := a.(type) {
	case int64:
		if y, ok := b.(int64); ok {
			return cmp3(x < y, x > y)
		}
	case float64:
		if y, ok := b.(float64); ok {
			return cmp3(x < y, x > y)
		}
	case string:
		if y, ok := b.(string); ok {
			return cmp3(x < y, x > y)
		}
	case bool:
		if y, ok := b.(bool); ok {
			return cmp3(!x && y, x && !y) // false < true
		}
	}
	return 0
}

func cmp3(less, greater bool) int {
	switch {
	case less:
		return -1
	case greater:
		return 1
	}
	return 0
}
