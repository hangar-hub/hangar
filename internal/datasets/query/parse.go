// Package query parses and evaluates the Dataset Viewer's /filter
// parameters (DATA-4): a `where` clause of comparisons on double-quoted
// columns joined by AND, OR, NOT and parentheses, and an `orderby` list.
// The input never reaches a SQL engine: it is parsed by this small
// recursive-descent parser into an expression tree that Hangar evaluates
// itself.
package query

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrSyntax means a clause does not parse.
var ErrSyntax = errors.New("syntax error")

// MaxLength caps the length of a clause.
const MaxLength = 16 << 10

// maxDepth caps nesting, so a clause of many parentheses cannot exhaust
// the stack.
const maxDepth = 64

type tokKind int

const (
	tokEOF tokKind = iota
	tokColumn
	tokString
	tokNumber
	tokWord
	tokOp
	tokLParen
	tokRParen
	tokComma
	tokDot
)

type token struct {
	kind tokKind
	text string
	pos  int
}

func lex(s string) ([]token, error) {
	if len(s) > MaxLength {
		return nil, fmt.Errorf("%w: the clause is longer than %d characters", ErrSyntax, MaxLength)
	}
	if !utf8.ValidString(s) {
		return nil, fmt.Errorf("%w: the clause is not valid UTF-8", ErrSyntax)
	}
	var out []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"' || c == '\'':
			start := i
			i++
			var b strings.Builder
			closed := false
			for i < len(s) {
				if s[i] == c {
					if i+1 < len(s) && s[i+1] == c {
						b.WriteByte(c)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("%w: unterminated quote at position %d", ErrSyntax, start)
			}
			kind := tokString
			if c == '"' {
				kind = tokColumn
				if b.Len() == 0 {
					return nil, fmt.Errorf("%w: empty column name at position %d", ErrSyntax, start)
				}
			}
			out = append(out, token{kind: kind, text: b.String(), pos: start})
		case c == '(':
			out = append(out, token{kind: tokLParen, text: "(", pos: i})
			i++
		case c == ')':
			out = append(out, token{kind: tokRParen, text: ")", pos: i})
			i++
		case c == ',':
			out = append(out, token{kind: tokComma, text: ",", pos: i})
			i++
		case c == '.' && (i+1 >= len(s) || !isDigit(s[i+1])):
			out = append(out, token{kind: tokDot, text: ".", pos: i})
			i++
		case c == '=' || c == '<' || c == '>' || c == '!':
			start := i
			op := string(c)
			if i+1 < len(s) && (s[i+1] == '=' || (c == '<' && s[i+1] == '>')) {
				op += string(s[i+1])
			}
			if op == "!" {
				return nil, fmt.Errorf("%w: unexpected '!' at position %d", ErrSyntax, start)
			}
			i += len(op)
			out = append(out, token{kind: tokOp, text: op, pos: start})
		case isDigit(c) || c == '.' || ((c == '-' || c == '+') && i+1 < len(s) && (isDigit(s[i+1]) || s[i+1] == '.')):
			start := i
			if c == '-' || c == '+' {
				i++
			}
			digits := 0
			for i < len(s) && isDigit(s[i]) {
				i++
				digits++
			}
			if i < len(s) && s[i] == '.' {
				i++
				for i < len(s) && isDigit(s[i]) {
					i++
					digits++
				}
			}
			if digits == 0 {
				return nil, fmt.Errorf("%w: malformed number at position %d", ErrSyntax, start)
			}
			if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
				j := i + 1
				if j < len(s) && (s[j] == '+' || s[j] == '-') {
					j++
				}
				if j < len(s) && isDigit(s[j]) {
					for j < len(s) && isDigit(s[j]) {
						j++
					}
					i = j
				}
			}
			out = append(out, token{kind: tokNumber, text: s[start:i], pos: start})
		case isLetter(c) || c == '_':
			start := i
			for i < len(s) && (isLetter(s[i]) || isDigit(s[i]) || s[i] == '_') {
				i++
			}
			out = append(out, token{kind: tokWord, text: strings.ToUpper(s[start:i]), pos: start})
		default:
			r, _ := utf8.DecodeRuneInString(s[i:])
			return nil, fmt.Errorf("%w: unexpected %q at position %d", ErrSyntax, r, i)
		}
	}
	return append(out, token{kind: tokEOF, pos: len(s)}), nil
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// Op is a comparison operator.
type Op string

// Operators.
const (
	OpEq    Op = "="
	OpNe    Op = "<>"
	OpLt    Op = "<"
	OpLe    Op = "<="
	OpGt    Op = ">"
	OpGe    Op = ">="
	OpNull  Op = "IS NULL"
	OpIn    Op = "IN"
	OpLike  Op = "LIKE"
	OpILike Op = "ILIKE"
)

// Literal is a constant in a clause.
type Literal struct {
	// Kind is "string", "number", "bool" or "null".
	Kind string
	Text string
	Bool bool
}

func (l Literal) String() string {
	switch l.Kind {
	case "string":
		return "'" + strings.ReplaceAll(l.Text, "'", "''") + "'"
	case "bool":
		if l.Bool {
			return "TRUE"
		}
		return "FALSE"
	case "null":
		return "NULL"
	}
	return l.Text
}

// Expr is a node of a where clause.
type Expr interface {
	String() string
}

// And is a conjunction.
type And struct{ Left, Right Expr }

// Or is a disjunction.
type Or struct{ Left, Right Expr }

// Not negates an expression.
type Not struct{ X Expr }

// Pred compares a column with literals.
type Pred struct {
	// Column is the path of the column: a name, then struct field names.
	Column []string
	Op     Op
	// Negate is set for <>, IS NOT NULL, NOT IN, NOT LIKE and NOT ILIKE.
	Negate bool
	Values []Literal
}

func quoteColumn(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

func (a *And) String() string { return "(" + a.Left.String() + " AND " + a.Right.String() + ")" }
func (o *Or) String() string  { return "(" + o.Left.String() + " OR " + o.Right.String() + ")" }
func (n *Not) String() string { return "(NOT " + n.X.String() + ")" }
func (p *Pred) String() string {
	col := quoteColumn(p.Column)
	switch p.Op {
	case OpNull:
		if p.Negate {
			return col + " IS NOT NULL"
		}
		return col + " IS NULL"
	case OpIn:
		vals := make([]string, len(p.Values))
		for i, v := range p.Values {
			vals[i] = v.String()
		}
		not := ""
		if p.Negate {
			not = "NOT "
		}
		return col + " " + not + "IN (" + strings.Join(vals, ", ") + ")"
	case OpLike, OpILike:
		not := ""
		if p.Negate {
			not = "NOT "
		}
		return col + " " + not + string(p.Op) + " " + p.Values[0].String()
	}
	return col + " " + string(p.Op) + " " + p.Values[0].String()
}

type parser struct {
	toks  []token
	i     int
	depth int
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) word(w string) bool {
	if t := p.peek(); t.kind == tokWord && t.text == w {
		p.next()
		return true
	}
	return false
}

func (p *parser) errorf(format string, args ...any) error {
	t := p.peek()
	what := t.text
	if t.kind == tokEOF {
		what = "the end"
	}
	return fmt.Errorf("%w at position %d (%s): %s", ErrSyntax, t.pos, what, fmt.Sprintf(format, args...))
}

// ParseWhere parses a where clause. An empty clause gives a nil Expr.
func ParseWhere(s string) (Expr, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	e, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, p.errorf("expected AND, OR or the end of the clause")
	}
	return e, nil
}

func (p *parser) or() (Expr, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.word("OR") {
		right, err := p.and()
		if err != nil {
			return nil, err
		}
		left = &Or{left, right}
	}
	return left, nil
}

func (p *parser) and() (Expr, error) {
	left, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.word("AND") {
		right, err := p.not()
		if err != nil {
			return nil, err
		}
		left = &And{left, right}
	}
	return left, nil
}

func (p *parser) not() (Expr, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxDepth {
		return nil, p.errorf("the clause is nested too deeply")
	}
	if p.word("NOT") {
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		return &Not{x}, nil
	}
	if p.peek().kind == tokLParen {
		p.next()
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, p.errorf("expected ')'")
		}
		p.next()
		return e, nil
	}
	return p.pred()
}

func (p *parser) column() ([]string, error) {
	t := p.peek()
	if t.kind != tokColumn {
		return nil, p.errorf("expected a double-quoted column name")
	}
	p.next()
	path := []string{t.text}
	for p.peek().kind == tokDot {
		p.next()
		t := p.peek()
		if t.kind != tokColumn {
			return nil, p.errorf("expected a double-quoted field name after '.'")
		}
		p.next()
		path = append(path, t.text)
	}
	return path, nil
}

func (p *parser) literal() (Literal, error) {
	t := p.peek()
	switch t.kind {
	case tokString:
		p.next()
		return Literal{Kind: "string", Text: t.text}, nil
	case tokNumber:
		p.next()
		return Literal{Kind: "number", Text: t.text}, nil
	case tokWord:
		switch t.text {
		case "TRUE", "FALSE":
			p.next()
			return Literal{Kind: "bool", Bool: t.text == "TRUE"}, nil
		case "NULL":
			p.next()
			return Literal{Kind: "null"}, nil
		}
	}
	return Literal{}, p.errorf("expected a string, a number, TRUE, FALSE or NULL")
}

func (p *parser) pred() (Expr, error) {
	col, err := p.column()
	if err != nil {
		return nil, err
	}
	pr := &Pred{Column: col}
	t := p.peek()
	switch {
	case t.kind == tokOp:
		p.next()
		switch t.text {
		case "=", "==":
			pr.Op = OpEq
		case "<>", "!=":
			pr.Op, pr.Negate = OpNe, true
		case "<":
			pr.Op = OpLt
		case "<=":
			pr.Op = OpLe
		case ">":
			pr.Op = OpGt
		case ">=":
			pr.Op = OpGe
		default:
			return nil, p.errorf("unknown operator %s", t.text)
		}
		lit, err := p.literal()
		if err != nil {
			return nil, err
		}
		pr.Values = []Literal{lit}
		return pr, nil
	case p.word("IS"):
		pr.Op = OpNull
		pr.Negate = p.word("NOT")
		if !p.word("NULL") {
			return nil, p.errorf("expected NULL after IS")
		}
		return pr, nil
	}
	pr.Negate = p.word("NOT")
	switch {
	case p.word("IN"):
		pr.Op = OpIn
		if p.peek().kind != tokLParen {
			return nil, p.errorf("expected '(' after IN")
		}
		p.next()
		for {
			lit, err := p.literal()
			if err != nil {
				return nil, err
			}
			pr.Values = append(pr.Values, lit)
			if p.peek().kind == tokComma {
				p.next()
				continue
			}
			break
		}
		if p.peek().kind != tokRParen {
			return nil, p.errorf("expected ',' or ')' in the IN list")
		}
		p.next()
		return pr, nil
	case p.word("LIKE"), p.word("ILIKE"):
		pr.Op = OpLike
		if p.toks[p.i-1].text == "ILIKE" {
			pr.Op = OpILike
		}
		t := p.peek()
		if t.kind != tokString {
			return nil, p.errorf("expected a quoted pattern after %s", pr.Op)
		}
		p.next()
		pr.Values = []Literal{{Kind: "string", Text: t.text}}
		return pr, nil
	}
	return nil, p.errorf("expected an operator: =, <>, !=, <, <=, >, >=, IS, IN, LIKE or ILIKE")
}

// Order is one orderby term.
type Order struct {
	Column []string
	Desc   bool
}

// ParseOrderBy parses an orderby list: columns, each optionally followed
// by ASC or DESC, separated by commas.
func ParseOrderBy(s string) ([]Order, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var out []Order
	for {
		col, err := p.column()
		if err != nil {
			return nil, err
		}
		o := Order{Column: col}
		if p.word("DESC") {
			o.Desc = true
		} else {
			p.word("ASC")
		}
		out = append(out, o)
		if p.peek().kind == tokComma {
			p.next()
			continue
		}
		if p.peek().kind != tokEOF {
			return nil, p.errorf("expected ',' or the end of the list")
		}
		return out, nil
	}
}
