package sql

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Commands (the parser's output)
// ---------------------------------------------------------------------------

// Command is any parsed statement.
type Command interface{ isCommand() }

// ColumnDef is a column name and type name (INT, TEXT, FLOAT or BOOL, uppercase).
type ColumnDef struct {
	Name string
	Type string
}

// Assignment is one "column = value" in UPDATE ... SET.
type Assignment struct {
	Column string
	Value  any
}

type CreateTable struct {
	Name    string
	Columns []ColumnDef
}

type DropTable struct{ Name string }

type AlterAddColumn struct {
	Table  string
	Column ColumnDef
}

type AlterDropColumn struct {
	Table  string
	Column string
}

type ShowTables struct{}

type Describe struct{ Table string }

// Insert: Columns is nil when the column list was omitted (values are then
// in table order). Each element of Rows is one row of literal values.
type Insert struct {
	Table   string
	Columns []string
	Rows    [][]any
}

// Select: Columns is nil for "*". Where is nil when there is no WHERE.
type Select struct {
	Table   string
	Columns []string
	Where   Expr
}

type Update struct {
	Table string
	Sets  []Assignment
	Where Expr
}

type Delete struct {
	Table string
	Where Expr
}

type Help struct{}
type Exit struct{}

func (CreateTable) isCommand()     {}
func (DropTable) isCommand()       {}
func (AlterAddColumn) isCommand()  {}
func (AlterDropColumn) isCommand() {}
func (ShowTables) isCommand()      {}
func (Describe) isCommand()        {}
func (Insert) isCommand()          {}
func (Select) isCommand()          {}
func (Update) isCommand()          {}
func (Delete) isCommand()          {}
func (Help) isCommand()            {}
func (Exit) isCommand()            {}

// ---------------------------------------------------------------------------
// WHERE expressions
// ---------------------------------------------------------------------------

// Expr is a WHERE condition.
type Expr interface{ isExpr() }

// Comparison is "column op value". Op is one of
// = != < > <= >= "IS NULL" "IS NOT NULL". For the IS forms Value is nil.
type Comparison struct {
	Column string
	Op     string
	Value  any
}

// Logical combines two conditions. Op is "AND" or "OR".
type Logical struct {
	Op          string
	Left, Right Expr
}

func (Comparison) isExpr() {}
func (Logical) isExpr()    {}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

// Parse parses one command. It returns (nil, nil) for empty input.
// Literal values in the result are int64, float64, string, bool or nil.
func Parse(input string) (Command, error) {
	toks, err := Lex(input)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}

	if p.peek().Kind == TokEOF || (p.peek().IsSymbol(";") && p.toks[1].Kind == TokEOF) {
		return nil, nil
	}

	cmd, err := p.parseCommand()
	if err != nil {
		return nil, err
	}
	p.acceptSymbol(";")
	if p.peek().Kind != TokEOF {
		return nil, p.errExpected("end of command")
	}
	return cmd, nil
}

type parser struct {
	toks []Token
	pos  int
}

func (p *parser) peek() Token { return p.toks[p.pos] }

func (p *parser) next() Token {
	t := p.toks[p.pos]
	if t.Kind != TokEOF {
		p.pos++
	}
	return t
}

func (p *parser) errExpected(what string) error {
	t := p.peek()
	return fmt.Errorf("expected %s but found %s (position %d)", what, t.Describe(), t.Pos)
}

func (p *parser) acceptKeyword(kw string) bool {
	if p.peek().IsKeyword(kw) {
		p.next()
		return true
	}
	return false
}

func (p *parser) acceptSymbol(sym string) bool {
	if p.peek().IsSymbol(sym) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectKeyword(kw string) error {
	if !p.acceptKeyword(kw) {
		return p.errExpected(kw)
	}
	return nil
}

func (p *parser) expectSymbol(sym string) error {
	if !p.acceptSymbol(sym) {
		return p.errExpected(fmt.Sprintf("%q", sym))
	}
	return nil
}

// expectIdent reads a name and lowercases it (names are case-insensitive).
func (p *parser) expectIdent(what string) (string, error) {
	if p.peek().Kind != TokIdent {
		return "", p.errExpected(what)
	}
	return strings.ToLower(p.next().Text), nil
}

func (p *parser) parseCommand() (Command, error) {
	t := p.peek()
	if t.Kind != TokIdent {
		return nil, p.errExpected("a command (try HELP)")
	}
	switch strings.ToUpper(t.Text) {
	case "CREATE":
		return p.parseCreate()
	case "DROP":
		return p.parseDrop()
	case "ALTER":
		return p.parseAlter()
	case "SHOW":
		p.next()
		if err := p.expectKeyword("TABLES"); err != nil {
			return nil, err
		}
		return ShowTables{}, nil
	case "DESCRIBE", "DESC":
		p.next()
		name, err := p.expectIdent("a table name")
		if err != nil {
			return nil, err
		}
		return Describe{Table: name}, nil
	case "INSERT":
		return p.parseInsert()
	case "SELECT":
		return p.parseSelect()
	case "UPDATE":
		return p.parseUpdate()
	case "DELETE":
		return p.parseDelete()
	case "HELP":
		p.next()
		return Help{}, nil
	case "EXIT", "QUIT":
		p.next()
		return Exit{}, nil
	}
	return nil, fmt.Errorf("unknown command %q (try HELP)", t.Text)
}

var typeNames = map[string]bool{"INT": true, "TEXT": true, "FLOAT": true, "BOOL": true}

func (p *parser) parseColumnDef() (ColumnDef, error) {
	name, err := p.expectIdent("a column name")
	if err != nil {
		return ColumnDef{}, err
	}
	if p.peek().Kind != TokIdent || !typeNames[strings.ToUpper(p.peek().Text)] {
		return ColumnDef{}, p.errExpected("a type (INT, TEXT, FLOAT or BOOL)")
	}
	return ColumnDef{Name: name, Type: strings.ToUpper(p.next().Text)}, nil
}

func (p *parser) parseCreate() (Command, error) {
	p.next() // CREATE
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	name, err := p.expectIdent("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectSymbol("("); err != nil {
		return nil, err
	}
	var cols []ColumnDef
	for {
		col, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		cols = append(cols, col)
		if !p.acceptSymbol(",") {
			break
		}
	}
	if err := p.expectSymbol(")"); err != nil {
		return nil, err
	}
	return CreateTable{Name: name, Columns: cols}, nil
}

func (p *parser) parseDrop() (Command, error) {
	p.next() // DROP
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	name, err := p.expectIdent("a table name")
	if err != nil {
		return nil, err
	}
	return DropTable{Name: name}, nil
}

func (p *parser) parseAlter() (Command, error) {
	p.next() // ALTER
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent("a table name")
	if err != nil {
		return nil, err
	}
	switch {
	case p.acceptKeyword("ADD"):
		if err := p.expectKeyword("COLUMN"); err != nil {
			return nil, err
		}
		col, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		return AlterAddColumn{Table: table, Column: col}, nil
	case p.acceptKeyword("DROP"):
		if err := p.expectKeyword("COLUMN"); err != nil {
			return nil, err
		}
		col, err := p.expectIdent("a column name")
		if err != nil {
			return nil, err
		}
		return AlterDropColumn{Table: table, Column: col}, nil
	}
	return nil, p.errExpected("ADD COLUMN or DROP COLUMN")
}

// parseLiteral reads a value: number, 'string', TRUE, FALSE or NULL.
func (p *parser) parseLiteral() (any, error) {
	t := p.peek()
	switch t.Kind {
	case TokInt:
		p.next()
		v, err := strconv.ParseInt(t.Text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("integer %s is out of range (position %d)", t.Text, t.Pos)
		}
		return v, nil
	case TokFloat:
		p.next()
		v, err := strconv.ParseFloat(t.Text, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %s (position %d)", t.Text, t.Pos)
		}
		return v, nil
	case TokString:
		p.next()
		return t.Text, nil
	case TokIdent:
		switch strings.ToUpper(t.Text) {
		case "TRUE":
			p.next()
			return true, nil
		case "FALSE":
			p.next()
			return false, nil
		case "NULL":
			p.next()
			return nil, nil
		}
	}
	return nil, p.errExpected("a value (number, 'text', TRUE, FALSE or NULL)")
}

func (p *parser) parseInsert() (Command, error) {
	p.next() // INSERT
	if err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent("a table name")
	if err != nil {
		return nil, err
	}

	var cols []string
	if p.acceptSymbol("(") {
		for {
			c, err := p.expectIdent("a column name")
			if err != nil {
				return nil, err
			}
			cols = append(cols, c)
			if !p.acceptSymbol(",") {
				break
			}
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
	}

	if err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	var rows [][]any
	for {
		if err := p.expectSymbol("("); err != nil {
			return nil, err
		}
		var row []any
		for {
			v, err := p.parseLiteral()
			if err != nil {
				return nil, err
			}
			row = append(row, v)
			if !p.acceptSymbol(",") {
				break
			}
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		rows = append(rows, row)
		if !p.acceptSymbol(",") {
			break
		}
	}
	return Insert{Table: table, Columns: cols, Rows: rows}, nil
}

func (p *parser) parseSelect() (Command, error) {
	p.next() // SELECT
	var cols []string
	if !p.acceptSymbol("*") {
		for {
			c, err := p.expectIdent("a column name or *")
			if err != nil {
				return nil, err
			}
			cols = append(cols, c)
			if !p.acceptSymbol(",") {
				break
			}
		}
	}
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent("a table name")
	if err != nil {
		return nil, err
	}
	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	return Select{Table: table, Columns: cols, Where: where}, nil
}

func (p *parser) parseUpdate() (Command, error) {
	p.next() // UPDATE
	table, err := p.expectIdent("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}
	var sets []Assignment
	for {
		col, err := p.expectIdent("a column name")
		if err != nil {
			return nil, err
		}
		if err := p.expectSymbol("="); err != nil {
			return nil, err
		}
		v, err := p.parseLiteral()
		if err != nil {
			return nil, err
		}
		sets = append(sets, Assignment{Column: col, Value: v})
		if !p.acceptSymbol(",") {
			break
		}
	}
	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	return Update{Table: table, Sets: sets, Where: where}, nil
}

func (p *parser) parseDelete() (Command, error) {
	p.next() // DELETE
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	table, err := p.expectIdent("a table name")
	if err != nil {
		return nil, err
	}
	where, err := p.parseOptionalWhere()
	if err != nil {
		return nil, err
	}
	return Delete{Table: table, Where: where}, nil
}

// ---------------------------------------------------------------------------
// WHERE: OR (lowest) < AND < comparison or ( ... )
// ---------------------------------------------------------------------------

func (p *parser) parseOptionalWhere() (Expr, error) {
	if !p.acceptKeyword("WHERE") {
		return nil, nil
	}
	return p.parseOr()
}

func (p *parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("OR") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = Logical{Op: "OR", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("AND") {
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		left = Logical{Op: "AND", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parsePrimary() (Expr, error) {
	if p.acceptSymbol("(") {
		e, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		return e, nil
	}

	col, err := p.expectIdent("a column name")
	if err != nil {
		return nil, err
	}

	if p.acceptKeyword("IS") {
		op := "IS NULL"
		if p.acceptKeyword("NOT") {
			op = "IS NOT NULL"
		}
		if err := p.expectKeyword("NULL"); err != nil {
			return nil, err
		}
		return Comparison{Column: col, Op: op}, nil
	}

	t := p.peek()
	if t.Kind != TokSymbol {
		return nil, p.errExpected("a comparison operator (= != < > <= >=)")
	}
	switch t.Text {
	case "=", "!=", "<", ">", "<=", ">=":
		p.next()
	default:
		return nil, p.errExpected("a comparison operator (= != < > <= >=)")
	}
	v, err := p.parseLiteral()
	if err != nil {
		return nil, err
	}
	return Comparison{Column: col, Op: t.Text, Value: v}, nil
}
