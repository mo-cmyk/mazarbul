package ladybug

import (
	"errors"
	"fmt"
	"time"

	lbug "github.com/LadybugDB/go-ladybug"
)

// result wraps an lbug.QueryResult and adapts the (HasNext, Next) cursor
// pattern to the kernel.GraphResult contract (Next, Scan, Err, Close).
//
// Next loads the next row into an internal cache; Scan assigns column
// values to caller-supplied pointers. The wrapper does not retain or
// expose the lbug FlatTuple beyond the lifetime of the current row.
type result struct {
	qr      *lbug.QueryResult
	tuple   *lbug.FlatTuple
	cached  []any
	err     error
	onClose func()
}

// Next advances to the next row. Returns false when the iterator is
// exhausted or when an error is observed; Err returns the latter.
func (r *result) Next() bool {
	if r.err != nil {
		return false
	}
	r.releaseTuple()
	if !r.qr.HasNext() {
		return false
	}
	t, err := r.qr.Next()
	if err != nil {
		r.err = fmt.Errorf("ladybug: next row: %w", err)
		return false
	}
	r.tuple = t
	values, err := t.GetAsSlice()
	if err != nil {
		r.err = fmt.Errorf("ladybug: scan row: %w", err)
		return false
	}
	r.cached = values
	return true
}

// Scan copies the current row's columns into the supplied destination
// pointers, in declaration order. Supported destination types: *string,
// *int64, *int, *float64, *bool, *time.Time, *any. The destination count
// must equal the column count.
func (r *result) Scan(into ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.tuple == nil {
		return errors.New("ladybug: Scan called before Next")
	}
	if len(into) != len(r.cached) {
		return fmt.Errorf("ladybug: Scan got %d destinations for %d columns", len(into), len(r.cached))
	}
	for i, dst := range into {
		if err := assign(dst, r.cached[i]); err != nil {
			return fmt.Errorf("ladybug: column %d: %w", i, err)
		}
	}
	return nil
}

// Err returns any deferred iteration error.
func (r *result) Err() error { return r.err }

// Close releases the lbug-side resources. Safe to call multiple times.
// Runs the onClose hook exactly once so callers can implement lifecycle
// tracking (e.g. Conn drains in-flight results before tearing down CGO
// state).
func (r *result) Close() error {
	r.releaseTuple()
	if r.qr != nil {
		r.qr.Close()
		r.qr = nil
	}
	if r.onClose != nil {
		r.onClose()
		r.onClose = nil
	}
	return nil
}

// values is an internal helper for migrate logic that needs to read a
// single row without going through Scan.
func (r *result) values() ([]any, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.tuple == nil {
		return nil, errors.New("ladybug: values called before Next")
	}
	return r.cached, nil
}

func (r *result) releaseTuple() {
	if r.tuple != nil {
		r.tuple.Close()
		r.tuple = nil
		r.cached = nil
	}
}

func assign(dst, src any) error {
	if d, ok := dst.(*any); ok {
		*d = src
		return nil
	}
	if src == nil {
		return errors.New("source is nil")
	}
	switch d := dst.(type) {
	case *string:
		return assignString(d, src)
	case *int64:
		return assignInt64(d, src)
	case *int:
		return assignInt(d, src)
	case *float64:
		return assignFloat64(d, src)
	case *bool:
		return assignBool(d, src)
	case *time.Time:
		return assignTime(d, src)
	default:
		return fmt.Errorf("unsupported destination type %T", dst)
	}
}

func assignString(dst *string, src any) error {
	s, ok := src.(string)
	if !ok {
		return fmt.Errorf("cannot assign %T to *string", src)
	}
	*dst = s
	return nil
}

// assignInt64 handles the two integer column widths lbug emits in practice:
// INT64 columns surface as Go int64, and a few system tables (e.g.
// show_tables) emit uint64. Other widths are added on demand.
func assignInt64(dst *int64, src any) error {
	switch v := src.(type) {
	case int64:
		*dst = v
	case uint64:
		*dst = int64(v)
	default:
		return fmt.Errorf("cannot assign %T to *int64", src)
	}
	return nil
}

func assignInt(dst *int, src any) error {
	switch v := src.(type) {
	case int64:
		*dst = int(v)
	case uint64:
		*dst = int(v)
	default:
		return fmt.Errorf("cannot assign %T to *int", src)
	}
	return nil
}

func assignFloat64(dst *float64, src any) error {
	v, ok := src.(float64)
	if !ok {
		return fmt.Errorf("cannot assign %T to *float64", src)
	}
	*dst = v
	return nil
}

func assignBool(dst *bool, src any) error {
	b, ok := src.(bool)
	if !ok {
		return fmt.Errorf("cannot assign %T to *bool", src)
	}
	*dst = b
	return nil
}

func assignTime(dst *time.Time, src any) error {
	t, ok := src.(time.Time)
	if !ok {
		return fmt.Errorf("cannot assign %T to *time.Time", src)
	}
	*dst = t
	return nil
}
