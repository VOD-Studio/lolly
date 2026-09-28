package hostmatch

import "testing"

func TestMatcher_ExactMatch(t *testing.T) {
	m := New[string]()
	_ = m.Add("example.com", "exact")

	v, ok := m.Find("example.com")
	if !ok || v != "exact" {
		t.Fatalf("Find() = %q, %v; want %q, true", v, ok, "exact")
	}
}

func TestMatcher_WildcardPrefix(t *testing.T) {
	m := New[string]()
	_ = m.Add("*.example.com", "wildcard-prefix")

	v, ok := m.Find("www.example.com")
	if !ok || v != "wildcard-prefix" {
		t.Fatalf("Find() = %q, %v; want %q, true", v, ok, "wildcard-prefix")
	}
}

func TestMatcher_WildcardSuffix(t *testing.T) {
	m := New[string]()
	_ = m.Add("example.*", "wildcard-suffix")

	v, ok := m.Find("example.org")
	if !ok || v != "wildcard-suffix" {
		t.Fatalf("Find() = %q, %v; want %q, true", v, ok, "wildcard-suffix")
	}
}

func TestMatcher_Regex(t *testing.T) {
	m := New[string]()
	_ = m.Add(`~^api-\d+\.example\.com$`, "regex")

	v, ok := m.Find("api-42.example.com")
	if !ok || v != "regex" {
		t.Fatalf("Find() = %q, %v; want %q, true", v, ok, "regex")
	}
}

func TestMatcher_Regex_InvalidPattern(t *testing.T) {
	m := New[string]()
	if err := m.Add("~(unclosed", "regex"); err == nil {
		t.Fatal("Add() error = nil, want error for invalid regex")
	}
}

func TestMatcher_Default(t *testing.T) {
	m := New[string]()
	m.SetDefault("default")

	v, ok := m.Find("unknown.example.com")
	if !ok || v != "default" {
		t.Fatalf("Find() = %q, %v; want %q, true", v, ok, "default")
	}
}

func TestMatcher_NoMatch_NoDefault(t *testing.T) {
	m := New[string]()

	v, ok := m.Find("unknown.example.com")
	if ok || v != "" {
		t.Fatalf("Find() = %q, %v; want %q, false", v, ok, "")
	}
}

func TestMatcher_Priority_ExactOverWildcard(t *testing.T) {
	m := New[string]()
	_ = m.Add("*.example.com", "wildcard")
	_ = m.Add("www.example.com", "exact")

	v, ok := m.Find("www.example.com")
	if !ok || v != "exact" {
		t.Fatalf("Find() = %q, %v; want %q, true (exact should win)", v, ok, "exact")
	}
}

func TestMatcher_Priority_LongestWildcardPrefix(t *testing.T) {
	m := New[string]()
	_ = m.Add("*.example.com", "short")
	_ = m.Add("*.b.example.com", "long")

	v, ok := m.Find("a.b.example.com")
	if !ok || v != "long" {
		t.Fatalf("Find() = %q, %v; want %q, true (longest suffix should win)", v, ok, "long")
	}
}

func TestMatcher_Priority_WildcardOverSuffix(t *testing.T) {
	m := New[string]()
	_ = m.Add("*.example.com", "prefix")
	_ = m.Add("example.*", "suffix")

	v, ok := m.Find("www.example.com")
	if !ok || v != "prefix" {
		t.Fatalf("Find() = %q, %v; want %q, true (prefix wildcard should win over suffix)", v, ok, "prefix")
	}
}

func TestMatcher_GenericValue(t *testing.T) {
	type payload struct{ name string }

	m := New[*payload]()
	_ = m.Add("example.com", &payload{name: "site"})

	v, ok := m.Find("example.com")
	if !ok || v == nil || v.name != "site" {
		t.Fatalf("Find() = %+v, %v; want site, true", v, ok)
	}

	v2, ok2 := m.Find("missing.com")
	if ok2 || v2 != nil {
		t.Fatalf("Find() = %+v, %v; want nil, false", v2, ok2)
	}
}
