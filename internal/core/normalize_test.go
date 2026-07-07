package core

import "testing"

func TestNormalizeText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"crlf to lf", "a\r\nb\r\n", "a\nb"},
		{"bare cr", "a\rb", "a\nb"},
		{"trailing spaces stripped", "a   \nb\t\n", "a\nb"},
		{"double-spaced lines collapse to single blank", "a\n\nb\n\nc", "a\n\nb\n\nc"},
		{"blank-line runs collapse", "a\n\n\n\nb", "a\n\nb"},
		{"blank lines of only whitespace collapse", "a\n \n \nb", "a\n\nb"},
		{"leading/trailing whitespace trimmed", "\n\n  a\n\n  ", "a"},
		{"content words preserved", "OrderService  \n\n\n writes to Kafka", "OrderService\n\n writes to Kafka"},
	}
	for _, c := range cases {
		if got := normalizeText(c.in); got != c.want {
			t.Errorf("%s: normalizeText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestNormalizeType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"writes_to", "writes_to"},
		{"writes-to", "writes_to"},
		{"writesTo", "writes_to"},
		{"Writes To", "writes_to"},
		{"  writes__to  ", "writes_to"},
		{"depends on", "depends_on"},
		{"DependsOn", "depends_on"},
		{"microservice", "microservice"}, // must NOT collapse into "service"
		{"", ""},
		{"-", ""}, // all-separator → empty (rejected upstream)
	}
	for _, c := range cases {
		if got := normalizeType(c.in); got != c.want {
			t.Errorf("normalizeType(%q) = %q, want %q", c.in, got, c.want)
		}
		if twice := normalizeType(normalizeType(c.in)); twice != normalizeType(c.in) {
			t.Errorf("normalizeType not idempotent for %q", c.in)
		}
	}
}

func TestNormalizeTextIdempotent(t *testing.T) {
	inputs := []string{
		"a\r\n\r\n\r\nb   \n\n",
		"line1\n \n \n \nline2\t\t\n",
		"already\n\nclean",
		"",
		"   \n\r\n  \t ",
	}
	for _, in := range inputs {
		once := normalizeText(in)
		twice := normalizeText(once)
		if once != twice {
			t.Errorf("not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}
