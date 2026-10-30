package filtersql_test

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate/filtersql"
)

func TestParse_EmptyIsNil(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "   ", "\t"} {
		got, err := filtersql.Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got != nil {
			t.Errorf("Parse(%q) = %v, want nil", in, got)
		}
	}
}

func TestParse_Comparison(t *testing.T) {
	t.Parallel()
	expr, err := filtersql.Parse(`kind = "person"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c, ok := expr.(filtersql.Comparison)
	if !ok {
		t.Fatalf("got %T, want Comparison", expr)
	}
	if c.Field != "kind" || c.Op != "=" || c.Value.Kind != filtersql.ValueString || c.Value.Str != "person" {
		t.Errorf("comparison = %+v", c)
	}
}

func TestParse_NumberAndConfidence(t *testing.T) {
	t.Parallel()
	expr, err := filtersql.Parse(`confidence >= 0.8`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := expr.(filtersql.Comparison)
	if c.Op != ">=" || c.Value.Kind != filtersql.ValueNumber || c.Value.Num != 0.8 {
		t.Errorf("comparison = %+v", c)
	}
}

// TestParse_PrecedenceORbindsTighter pins the AIP-160 rule: a AND b OR c parses
// as a AND (b OR c).
func TestParse_PrecedenceORbindsTighter(t *testing.T) {
	t.Parallel()
	expr, err := filtersql.Parse(`a = "1" AND b = "2" OR c = "3"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	and, ok := expr.(filtersql.And)
	if !ok {
		t.Fatalf("top node = %T, want And", expr)
	}
	if len(and.Operands) != 2 {
		t.Fatalf("And operands = %d, want 2", len(and.Operands))
	}
	if _, ok := and.Operands[0].(filtersql.Comparison); !ok {
		t.Errorf("operand[0] = %T, want Comparison (a)", and.Operands[0])
	}
	or, ok := and.Operands[1].(filtersql.Or)
	if !ok {
		t.Fatalf("operand[1] = %T, want Or (b OR c)", and.Operands[1])
	}
	if len(or.Operands) != 2 {
		t.Errorf("Or operands = %d, want 2", len(or.Operands))
	}
}

func TestParse_ParensOverridePrecedence(t *testing.T) {
	t.Parallel()
	expr, err := filtersql.Parse(`(a = "1" OR b = "2") AND c = "3"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	and, ok := expr.(filtersql.And)
	if !ok {
		t.Fatalf("top = %T, want And", expr)
	}
	if _, ok := and.Operands[0].(filtersql.Or); !ok {
		t.Errorf("operand[0] = %T, want Or", and.Operands[0])
	}
}

func TestParse_Not(t *testing.T) {
	t.Parallel()
	expr, err := filtersql.Parse(`NOT kind = "person"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := expr.(filtersql.Not); !ok {
		t.Fatalf("got %T, want Not", expr)
	}
}

func TestParse_Function(t *testing.T) {
	t.Parallel()
	expr, err := filtersql.Parse(`related("works_at", "Mithril Corp")`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	call, ok := expr.(filtersql.Call)
	if !ok {
		t.Fatalf("got %T, want Call", expr)
	}
	if call.Name != "related" || len(call.Args) != 2 {
		t.Fatalf("call = %+v", call)
	}
	if call.Args[0].Str != "works_at" || call.Args[1].Str != "Mithril Corp" {
		t.Errorf("args = %+v", call.Args)
	}
}

func TestParse_EscapedQuoteInString(t *testing.T) {
	t.Parallel()
	expr, err := filtersql.Parse(`name = "say \"hi\""`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := expr.(filtersql.Comparison).Value.Str; got != `say "hi"` {
		t.Errorf("value = %q, want %q", got, `say "hi"`)
	}
}

func TestParse_DottedStateKey(t *testing.T) {
	t.Parallel()
	expr, err := filtersql.Parse(`state.task.status = "pending"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := expr.(filtersql.Comparison).Field; got != "state.task.status" {
		t.Errorf("field = %q, want state.task.status", got)
	}
}

func TestParse_Errors(t *testing.T) {
	t.Parallel()
	bad := []string{
		`kind`,                 // field with no comparator
		`kind =`,               // comparator with no value
		`= "x"`,                // no field
		`kind = "x" AND`,       // dangling AND
		`(kind = "x"`,          // unclosed paren
		`name = "unterminated`, // unterminated string
		`kind = "x" extra`,     // trailing junk
		`related("x"`,          // unclosed call
	}
	for _, in := range bad {
		if _, err := filtersql.Parse(in); err == nil {
			t.Errorf("Parse(%q): expected error, got nil", in)
		}
	}
}
