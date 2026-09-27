package config

import "testing"

func TestNormalizeBaseURLKeepsIPv6Brackets(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"http://[::1]", "http://[::1]"},
		{"https://[::1]:443/v1/", "https://[::1]:443/v1"},
		{"[https://[::1]:443/v1/]", "https://[::1]:443/v1"},
		{"[https://example.test/v1/]", "https://example.test/v1"},
	} {
		if got := NormalizeBaseURL(tc.input); got != tc.want {
			t.Fatalf("NormalizeBaseURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
		if err := ValidateBaseURL(tc.input); err != nil {
			t.Fatal(err)
		}
	}
}
