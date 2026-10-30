// Package filtersql parses an AIP-160-style filter expression into an abstract
// syntax tree. It implements the subset of https://google.aip.dev/160 that the
// structured entity-query surface needs: comparisons (= != < > <= >= :),
// boolean AND / OR / NOT with parentheses, function calls (e.g.
// related("works_at", "Mithril")), and string / number / bare-identifier values.
//
// Parsing is intentionally separate from SQL emission: this package is pure and
// schema-agnostic (no database, no knowledge of entities or facts), so it is
// trivially unit-testable, and the navigate package owns the AST → SQL walk
// where the entity / fact / relation field mapping lives.
//
// Precedence follows AIP-160 exactly: OR binds tighter than AND, so
// `a AND b OR c` parses as `a AND (b OR c)`. Use explicit parentheses when the
// intent is otherwise.
package filtersql

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Expr is a node in the parsed filter tree.
type Expr interface{ exprNode() }

// And is the conjunction of two or more operands (the lowest-precedence join).
type And struct{ Operands []Expr }

// Or is the disjunction of two or more operands (binds tighter than And).
type Or struct{ Operands []Expr }

// Not negates its operand.
type Not struct{ Operand Expr }

// Comparison is `field <op> value`, where op is one of = != < > <= >= :.
type Comparison struct {
	Field string
	Op    string
	Value Value
}

// Call is a function restriction, e.g. related("works_at", "Mithril Corp").
type Call struct {
	Name string
	Args []Value
}

func (And) exprNode()        {}
func (Or) exprNode()         {}
func (Not) exprNode()        {}
func (Comparison) exprNode() {}
func (Call) exprNode()       {}

// ValueKind discriminates a literal value.
type ValueKind int

const (
	// ValueString is a double-quoted string literal.
	ValueString ValueKind = iota
	// ValueNumber is an integer or float literal.
	ValueNumber
	// ValueIdent is a bare identifier used as a value (e.g. an enum token).
	ValueIdent
)

// Value is a literal operand of a comparison or function argument.
type Value struct {
	Kind ValueKind
	Str  string  // set for ValueString / ValueIdent
	Num  float64 // set for ValueNumber
}

// Parse parses an AIP-160-subset filter string into an Expr. An empty (or
// whitespace-only) filter returns (nil, nil): callers treat a nil Expr as "no
// filter". A malformed filter returns a descriptive error.
func Parse(filter string) (Expr, error) {
	toks, err := lex(filter)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		//nolint:nilnil // (nil, nil) is the documented "no filter" signal for an empty input
		return nil, nil
	}
	p := &parser{toks: toks}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if !p.atEnd() {
		return nil, fmt.Errorf("filter: unexpected %q at position %d", p.cur().val, p.cur().pos)
	}
	return expr, nil
}

// --- lexer ---

type tokenKind int

const (
	tokIdent tokenKind = iota
	tokString
	tokNumber
	tokOp
	tokLParen
	tokRParen
	tokComma
)

type token struct {
	kind tokenKind
	val  string
	pos  int
}

func lex(s string) ([]token, error) {
	var toks []token
	runes := []rune(s)
	i := 0
	for i < len(runes) {
		c := runes[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '(':
			toks = append(toks, token{tokLParen, "(", i})
			i++
		case c == ')':
			toks = append(toks, token{tokRParen, ")", i})
			i++
		case c == ',':
			toks = append(toks, token{tokComma, ",", i})
			i++
		case c == '"':
			str, next, err := lexString(runes, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tokString, str, i})
			i = next
		default:
			if op := matchComparator(runes, i); op != "" {
				toks = append(toks, token{tokOp, op, i})
				i += len([]rune(op))
				continue
			}
			if isNumberStart(runes, i) {
				num, next := lexNumber(runes, i)
				toks = append(toks, token{tokNumber, num, i})
				i = next
				continue
			}
			if isIdentRune(c) {
				ident, next := lexIdent(runes, i)
				toks = append(toks, token{tokIdent, ident, i})
				i = next
				continue
			}
			return nil, fmt.Errorf("filter: unexpected character %q at position %d", string(c), i)
		}
	}
	return toks, nil
}

func lexString(runes []rune, start int) (string, int, error) {
	var b strings.Builder
	i := start + 1 // skip opening quote
	for i < len(runes) {
		c := runes[i]
		switch c {
		case '\\':
			if i+1 >= len(runes) {
				return "", 0, fmt.Errorf("filter: dangling escape at position %d", i)
			}
			b.WriteRune(runes[i+1]) // \" and \\ both yield the next rune verbatim
			i += 2
		case '"':
			return b.String(), i + 1, nil
		default:
			b.WriteRune(c)
			i++
		}
	}
	return "", 0, fmt.Errorf("filter: unterminated string starting at position %d", start)
}

func matchComparator(runes []rune, i int) string {
	// Longest first so <= / >= / != win over their single-char prefixes.
	comparators := []string{"<=", ">=", "!=", "=", "<", ">", ":"}
	for _, op := range comparators {
		or := []rune(op)
		if i+len(or) > len(runes) {
			continue
		}
		if string(runes[i:i+len(or)]) == op {
			return op
		}
	}
	return ""
}

func isNumberStart(runes []rune, i int) bool {
	c := runes[i]
	if unicode.IsDigit(c) {
		return true
	}
	// A leading '-' is a number only when followed by a digit; otherwise it is
	// not part of this subset's vocabulary.
	return c == '-' && i+1 < len(runes) && unicode.IsDigit(runes[i+1])
}

func lexNumber(runes []rune, start int) (string, int) {
	i := start
	if runes[i] == '-' {
		i++
	}
	for i < len(runes) && (unicode.IsDigit(runes[i]) || runes[i] == '.') {
		i++
	}
	return string(runes[start:i]), i
}

// isIdentRune reports whether c may appear in a bare identifier. Dots are
// allowed so a hierarchical state_key (state.task.status) lexes as one token.
func isIdentRune(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '.'
}

func lexIdent(runes []rune, start int) (string, int) {
	i := start
	for i < len(runes) && isIdentRune(runes[i]) {
		i++
	}
	return string(runes[start:i]), i
}

// --- parser ---

type parser struct {
	toks []token
	pos  int
}

func (p *parser) atEnd() bool    { return p.pos >= len(p.toks) }
func (p *parser) cur() token     { return p.toks[p.pos] }
func (p *parser) advance() token { t := p.toks[p.pos]; p.pos++; return t }

// isKeyword reports whether the current token is the bare keyword kw (AND / OR /
// NOT are uppercase identifiers in AIP-160).
func (p *parser) isKeyword(kw string) bool {
	return !p.atEnd() && p.cur().kind == tokIdent && p.cur().val == kw
}

// parseExpression = factor (AND factor)*  — AND is the lowest-precedence join.
func (p *parser) parseExpression() (Expr, error) {
	first, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	operands := []Expr{first}
	for p.isKeyword("AND") {
		p.advance()
		next, ferr := p.parseFactor()
		if ferr != nil {
			return nil, ferr
		}
		operands = append(operands, next)
	}
	if len(operands) == 1 {
		return operands[0], nil
	}
	return And{Operands: operands}, nil
}

// parseFactor = term (OR term)*  — OR binds tighter than AND per AIP-160.
func (p *parser) parseFactor() (Expr, error) {
	first, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	operands := []Expr{first}
	for p.isKeyword("OR") {
		p.advance()
		next, terr := p.parseTerm()
		if terr != nil {
			return nil, terr
		}
		operands = append(operands, next)
	}
	if len(operands) == 1 {
		return operands[0], nil
	}
	return Or{Operands: operands}, nil
}

// parseTerm = NOT term | "(" expression ")" | restriction.
func (p *parser) parseTerm() (Expr, error) {
	if p.isKeyword("NOT") {
		p.advance()
		inner, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		return Not{Operand: inner}, nil
	}
	if !p.atEnd() && p.cur().kind == tokLParen {
		p.advance()
		inner, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if p.atEnd() || p.cur().kind != tokRParen {
			return nil, errors.New("filter: expected ')' to close group")
		}
		p.advance()
		return inner, nil
	}
	return p.parseRestriction()
}

// parseRestriction = function | comparison.
func (p *parser) parseRestriction() (Expr, error) {
	if p.atEnd() || p.cur().kind != tokIdent {
		if p.atEnd() {
			return nil, errors.New("filter: unexpected end of input; expected a field or function")
		}
		return nil, fmt.Errorf("filter: expected a field or function, got %q at position %d", p.cur().val, p.cur().pos)
	}
	ident := p.advance()
	// function call: IDENT "(" args ")"
	if !p.atEnd() && p.cur().kind == tokLParen {
		return p.parseCall(ident.val)
	}
	// comparison: IDENT comparator value
	if p.atEnd() || p.cur().kind != tokOp {
		return nil, fmt.Errorf("filter: expected a comparator after %q at position %d", ident.val, ident.pos)
	}
	op := p.advance().val
	val, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	return Comparison{Field: ident.val, Op: op, Value: val}, nil
}

func (p *parser) parseCall(name string) (Expr, error) {
	p.advance() // consume "("
	var args []Value
	if !p.atEnd() && p.cur().kind != tokRParen {
		for {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args = append(args, v)
			if p.atEnd() || p.cur().kind != tokComma {
				break
			}
			p.advance() // consume ","
		}
	}
	if p.atEnd() || p.cur().kind != tokRParen {
		return nil, fmt.Errorf("filter: expected ')' to close %s(...)", name)
	}
	p.advance() // consume ")"
	return Call{Name: name, Args: args}, nil
}

func (p *parser) parseValue() (Value, error) {
	if p.atEnd() {
		return Value{}, errors.New("filter: expected a value")
	}
	t := p.advance()
	switch t.kind {
	case tokString:
		return Value{Kind: ValueString, Str: t.val}, nil
	case tokNumber:
		n, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return Value{}, fmt.Errorf("filter: invalid number %q at position %d", t.val, t.pos)
		}
		return Value{Kind: ValueNumber, Num: n}, nil
	case tokIdent:
		return Value{Kind: ValueIdent, Str: t.val}, nil
	default:
		return Value{}, fmt.Errorf("filter: expected a value, got %q at position %d", t.val, t.pos)
	}
}
