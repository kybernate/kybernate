package version

import "testing"

func TestString(t *testing.T) {
	Version, Commit = "v1.2.3", "abc123"
	t.Cleanup(func() { Version, Commit = "dev", "unknown" })

	if got, want := String(), "v1.2.3 (commit abc123)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
