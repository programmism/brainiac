package main

import "testing"

func TestWebURL(t *testing.T) {
	cases := map[string]string{
		":8080":          "http://localhost:8080",
		"0.0.0.0:8080":   "http://localhost:8080",
		"[::]:8080":      "http://localhost:8080",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
		"example.com:80": "http://example.com:80",
		"8080":           "http://localhost8080", // no colon → treated as host-less addr
	}
	for addr, want := range cases {
		if got := webURL(addr); got != want {
			t.Errorf("webURL(%q) = %q, want %q", addr, got, want)
		}
	}
}
