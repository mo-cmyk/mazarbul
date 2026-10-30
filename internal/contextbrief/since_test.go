package contextbrief

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// ─── parseRFC3339Time ─────────────────────────────────────────────────────────

func TestParseRFC3339Time_ValidRFC3339(t *testing.T) {
	t.Parallel()
	s := "2025-01-15T10:00:00Z"
	got := parseRFC3339Time(s)
	if got.IsZero() {
		t.Fatalf("parseRFC3339Time(%q) returned zero", s)
	}
	if got.Year() != 2025 || got.Month() != 1 || got.Day() != 15 {
		t.Errorf("parseRFC3339Time(%q) = %v, want 2025-01-15", s, got)
	}
}

func TestParseRFC3339Time_ValidRFC3339Nano(t *testing.T) {
	t.Parallel()
	s := "2025-03-01T09:30:00.123456789Z"
	got := parseRFC3339Time(s)
	if got.IsZero() {
		t.Fatalf("parseRFC3339Time(%q) returned zero", s)
	}
}

func TestParseRFC3339Time_Empty(t *testing.T) {
	t.Parallel()
	if got := parseRFC3339Time(""); !got.IsZero() {
		t.Errorf("parseRFC3339Time(\"\") = %v, want zero", got)
	}
}

func TestParseRFC3339Time_Invalid(t *testing.T) {
	t.Parallel()
	if got := parseRFC3339Time("not-a-date"); !got.IsZero() {
		t.Errorf("parseRFC3339Time(invalid) = %v, want zero", got)
	}
}

// ─── filterEntitiesByCreateTime ───────────────────────────────────────────────

func TestFilterEntitiesByCreateTime_KeepsNew(t *testing.T) {
	t.Parallel()
	since := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	ents := []navigate.Entity{
		{UID: "old", CreateTime: "2025-05-01T00:00:00Z"},
		{UID: "new", CreateTime: "2025-06-15T00:00:00Z"},
		{UID: "exact", CreateTime: "2025-06-01T00:00:00Z"},
	}
	got := filterEntitiesByCreateTime(ents, since)
	uids := entityUIDs(got)
	if contains(uids, "old") {
		t.Error("old entity (before since) should be filtered out")
	}
	if !contains(uids, "new") {
		t.Error("new entity (after since) should be kept")
	}
	if !contains(uids, "exact") {
		t.Error("entity at exactly since should be kept (inclusive)")
	}
}

func TestFilterEntitiesByCreateTime_EmptyTimestampKept(t *testing.T) {
	t.Parallel()
	since := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	ents := []navigate.Entity{
		{UID: "no-time", CreateTime: ""},
	}
	got := filterEntitiesByCreateTime(ents, since)
	if len(got) != 1 {
		t.Errorf("entity with empty createTime should be kept (permissive), got %d", len(got))
	}
}

func TestFilterEntitiesByCreateTime_ZeroSinceNoop(t *testing.T) {
	t.Parallel()
	ents := []navigate.Entity{
		{UID: "a", CreateTime: "2020-01-01T00:00:00Z"},
		{UID: "b", CreateTime: "2025-01-01T00:00:00Z"},
	}
	got := filterEntitiesByCreateTime(ents, time.Time{})
	if len(got) != len(ents) {
		t.Errorf("zero since should keep all entities, got %d want %d", len(got), len(ents))
	}
}

// ─── filterDocsByUpdateTime ───────────────────────────────────────────────────

func TestFilterDocsByUpdateTime_KeepsRecent(t *testing.T) {
	t.Parallel()
	since := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	docs := []navigate.Document{
		{DocumentUID: "old", UpdateTime: "2025-05-01T00:00:00Z"},
		{DocumentUID: "recent", UpdateTime: "2025-06-15T00:00:00Z"},
		{DocumentUID: "exact", UpdateTime: "2025-06-01T00:00:00Z"},
	}
	got := filterDocsByUpdateTime(docs, since)
	duids := docUIDs(got)
	if contains(duids, "old") {
		t.Error("old doc (before since) should be filtered out")
	}
	if !contains(duids, "recent") {
		t.Error("recent doc (after since) should be kept")
	}
	if !contains(duids, "exact") {
		t.Error("doc at exactly since should be kept (inclusive)")
	}
}

func TestFilterDocsByUpdateTime_FallsBackToCreateTime(t *testing.T) {
	t.Parallel()
	since := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	docs := []navigate.Document{
		// No UpdateTime: fall back to CreateTime.
		{DocumentUID: "new-no-update", CreateTime: "2025-06-10T00:00:00Z", UpdateTime: ""},
		{DocumentUID: "old-no-update", CreateTime: "2025-05-10T00:00:00Z", UpdateTime: ""},
	}
	got := filterDocsByUpdateTime(docs, since)
	duids := docUIDs(got)
	if !contains(duids, "new-no-update") {
		t.Error("doc with new createTime (no updateTime) should be kept")
	}
	if contains(duids, "old-no-update") {
		t.Error("doc with old createTime (no updateTime) should be filtered")
	}
}

func TestFilterDocsByUpdateTime_EmptyTimestampKept(t *testing.T) {
	t.Parallel()
	since := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	docs := []navigate.Document{
		{DocumentUID: "no-time"},
	}
	got := filterDocsByUpdateTime(docs, since)
	if len(got) != 1 {
		t.Errorf("doc with no timestamp should be kept (permissive), got %d", len(got))
	}
}

// ─── Build with Since ─────────────────────────────────────────────────────────

func TestBuild_SinceFiltersEntities(t *testing.T) {
	t.Parallel()
	since := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		entities: []navigate.Entity{
			{UID: "old-ent", Kind: "person", DisplayName: "OldPerson", CreateTime: "2025-05-01T00:00:00Z"},
			{UID: "new-ent", Kind: "person", DisplayName: "NewPerson", CreateTime: "2025-06-15T00:00:00Z"},
		},
	}
	a := New(reader, nil)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000, Since: since})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	if strings.Contains(md, "OldPerson") {
		t.Errorf("entity created before Since should be absent from brief:\n%s", md)
	}
	if !strings.Contains(md, "NewPerson") {
		t.Errorf("entity created after Since should appear in brief:\n%s", md)
	}
}

func TestBuild_SinceFiltersRecentDocs(t *testing.T) {
	t.Parallel()
	since := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		recent: []navigate.Document{
			{DocumentUID: "old-doc", Path: "notes/old.md", Title: "Old Doc", UpdateTime: "2025-05-01T00:00:00Z"},
			{DocumentUID: "new-doc", Path: "notes/new.md", Title: "New Doc", UpdateTime: "2025-06-15T00:00:00Z"},
		},
	}
	a := New(reader, nil)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000, Since: since})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	md := brief.Markdown
	if strings.Contains(md, "notes/old.md") {
		t.Errorf("doc updated before Since should be absent from brief:\n%s", md)
	}
	if !strings.Contains(md, "notes/new.md") {
		t.Errorf("doc updated after Since should appear in brief:\n%s", md)
	}
}

func TestBuild_SinceTitleAnnotation(t *testing.T) {
	t.Parallel()
	since := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	a := New(&fakeReader{}, nil)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000, Since: since})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(brief.Markdown, "since") {
		t.Errorf("brief title should mention 'since' when Since is set:\n%s", brief.Markdown)
	}
	if !strings.Contains(brief.Markdown, "2025-06-01T00:00:00Z") {
		t.Errorf("brief title should include the since timestamp:\n%s", brief.Markdown)
	}
}

func TestBuild_ZeroSince_NoAnnotation(t *testing.T) {
	t.Parallel()
	a := New(&fakeReader{}, nil)
	brief, err := a.Build(context.Background(), Request{BudgetTokens: 4000})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(brief.Markdown, "since") {
		t.Errorf("brief title should not mention 'since' when Since is zero:\n%s", brief.Markdown)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func entityUIDs(ents []navigate.Entity) []string {
	out := make([]string, len(ents))
	for i := range ents {
		out[i] = ents[i].UID
	}
	return out
}

func docUIDs(docs []navigate.Document) []string {
	out := make([]string, len(docs))
	for i := range docs {
		out[i] = docs[i].DocumentUID
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
