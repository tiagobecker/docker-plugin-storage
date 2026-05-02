package imagefs

import "testing"

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"64m": 64 * 1024 * 1024,
		"2G":  2 * 1024 * 1024 * 1024,
		"1gb": 1000 * 1000 * 1000,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil {
			t.Fatalf("ParseSize(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseSize(%q)=%d, want %d", in, got, want)
		}
	}
}

func TestParseSizeRejectsInvalidValues(t *testing.T) {
	for _, in := range []string{"", "0", "-1g", "abc"} {
		if _, err := ParseSize(in); err == nil {
			t.Fatalf("ParseSize(%q) should fail", in)
		}
	}
}
