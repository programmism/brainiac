package model

import "testing"

func TestScopeKeyDeterministicAndOrderIndependent(t *testing.T) {
	a := ScopeKey(map[string]string{"project": "goroutly", "env": "prod"})
	b := ScopeKey(map[string]string{"env": "prod", "project": "goroutly"})
	if a != b {
		t.Fatalf("scope_key must be order-independent: %q vs %q", a, b)
	}
	if got := ScopeKey(nil); got != "" {
		t.Fatalf("empty discriminators must be global (\"\"), got %q", got)
	}
	// Distinct sets must not collide.
	if ScopeKey(map[string]string{"project": "a"}) == ScopeKey(map[string]string{"project": "b"}) {
		t.Fatal("different values collided")
	}
}

func TestValidateDiscriminators(t *testing.T) {
	ok := []map[string]string{
		nil,
		{"project": "goroutly"},
		{"project": "goroutly", "env": "prod"},
	}
	for _, d := range ok {
		if err := ValidateDiscriminators(d); err != nil {
			t.Errorf("valid %v rejected: %v", d, err)
		}
	}
	bad := []map[string]string{
		{"": "v"},               // empty key
		{"env": ""},             // empty value
		{"env": "a;b"},          // delimiter in value
		{"a=b": "c"},            // delimiter in key
		{"env": "prod=staging"}, // '=' in value
	}
	for _, d := range bad {
		if err := ValidateDiscriminators(d); err == nil {
			t.Errorf("invalid %v accepted", d)
		}
	}
}

func TestScopeLabel(t *testing.T) {
	cases := []struct {
		disc map[string]string
		want string
	}{
		{nil, "global"},
		{map[string]string{}, "global"},
		{map[string]string{"project": "neznaika"}, "project:neznaika"},
		{map[string]string{"env": "prod"}, "env=prod"},                           // non-project single axis → scope_key
		{map[string]string{"project": "x", "env": "prod"}, "env=prod;project=x"}, // multi-axis → scope_key
	}
	for _, c := range cases {
		if got := ScopeLabel(c.disc); got != c.want {
			t.Errorf("ScopeLabel(%v) = %q, want %q", c.disc, got, c.want)
		}
	}
}
