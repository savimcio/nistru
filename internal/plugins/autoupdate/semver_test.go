package autoupdate

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{"minor-less", "v0.1.0", "v0.2.0", -1},
		{"minor-greater", "v0.2.0", "v0.1.0", 1},
		{"equal", "v0.2.0", "v0.2.0", 0},
		{"stable-gt-prerelease", "v0.2.0", "v0.2.0-dev.20260421-abc1234", 1},
		{"prerelease-ordering", "v0.2.0-dev.20260420", "v0.2.0-dev.20260421", -1},
		{"missing-v-prefix-equal", "0.1.0", "v0.1.0", 0},
		{"unknown-lt-known", "unknown", "v0.1.0", -1},
		{"known-gt-unknown", "v0.1.0", "unknown", 1},
		{"both-unknown", "unknown", "unknown", 0},
		{"empty-lt-known", "", "v0.1.0", -1},
		{"devel-lt-known", "(devel)", "v0.1.0", -1},
		{"garbage-lt-known", "garbage", "v0.1.0", -1},
		{"known-gt-garbage", "v0.1.0", "garbage", 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareVersions(tc.a, tc.b)
			if got != tc.want {
				t.Fatalf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"missing-v-prefix", "0.1.0", "v0.1.0"},
		{"already-canonical", "v0.1.0", "v0.1.0"},
		{"short-form-canonicalized", "v0.1", "v0.1.0"},
		{"invalid-passthrough", "garbage", "garbage"},
		{"empty-passthrough", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeVersion(tc.in)
			if got != tc.want {
				t.Fatalf("NormalizeVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
