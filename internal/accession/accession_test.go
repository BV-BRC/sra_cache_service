package accession

import "testing"

func TestValidateAcceptsEveryKnownPrefix(t *testing.T) {
	for _, prefix := range Prefixes {
		for _, digits := range []string{"123456", "1234567890", "123456789012"} {
			acc := prefix + digits
			if err := Validate(acc); err != nil {
				t.Errorf("Validate(%q) = %v, want nil", acc, err)
			}
		}
	}
}

func TestValidateRejectsUnknownPrefix(t *testing.T) {
	cases := []string{
		"XYZ123456",  // not one of the nine prefixes
		"srr123456",  // lowercase
		"Srr123456",  // mixed case
		"GCA_123456", // a genome accession, not an SRA one
		"",           // empty
	}
	for _, acc := range cases {
		if err := Validate(acc); err == nil {
			t.Errorf("Validate(%q) = nil, want an error", acc)
		}
	}
}

func TestValidateRejectsNonDigitSuffix(t *testing.T) {
	cases := []string{
		"SRR12345X",
		"SRRabcdef",
		"SRR123 456",
		"SRR123456 ",
		"SRR123-456",
	}
	for _, acc := range cases {
		if err := Validate(acc); err == nil {
			t.Errorf("Validate(%q) = nil, want an error", acc)
		}
	}
}

func TestValidateDigitLengthBounds(t *testing.T) {
	cases := []struct {
		acc     string
		wantErr bool
	}{
		{"SRR1", true},               // way too short
		{"SRR12345", true},           // 5 digits, one under the 6-digit floor
		{"SRR123456", false},         // 6 digits, the floor
		{"SRR123456789012", false},   // 12 digits, the ceiling (len("123456789012")==12)
		{"SRR1234567890123", true},   // 13 digits, one over the ceiling
		{"SRR" + rep("1", 20), true}, // absurdly long
	}
	for _, c := range cases {
		err := Validate(c.acc)
		if (err != nil) != c.wantErr {
			t.Errorf("Validate(%q) error = %v, wantErr = %v", c.acc, err, c.wantErr)
		}
	}
}

func TestIsRunLevel(t *testing.T) {
	cases := []struct {
		acc  string
		want bool
	}{
		{"SRR123456", true},
		{"DRR123456", true},
		{"ERR123456", true},
		{"SRX123456", false},
		{"SRP123456", false},
		{"DRX123456", false},
		{"DRP123456", false},
		{"ERX123456", false},
		{"ERP123456", false},
		{"garbage", false},
		{"SR", false}, // too short to even have a 3-char prefix
		{"", false},
	}
	for _, c := range cases {
		if got := IsRunLevel(c.acc); got != c.want {
			t.Errorf("IsRunLevel(%q) = %v, want %v", c.acc, got, c.want)
		}
	}
}

// rep returns s repeated n times.
func rep(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
