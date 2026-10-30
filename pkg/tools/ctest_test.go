package tools

import "testing"

func TestNormalizeCTestCommand_FilterTestAndTimeout(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			"ctest --test-dir build --output-on-failure --filter-test cli",
			"ctest --timeout 30 --test-dir build --output-on-failure -R cli",
		},
		{
			"ctest --filter-test protocol --output-on-failure",
			"ctest --timeout 30 -R protocol --output-on-failure",
		},
		{
			"ctest --test-dir build --output-on-failure -R integrate",
			"ctest --timeout 30 --test-dir build --output-on-failure -R integrate",
		},
		{
			"ctest --timeout 10 --filter-test cli",
			"ctest --timeout 10 -R cli",
		},
		{
			"cmake -S . -B build && ctest --test-dir build --output-on-failure --filter-test x",
			"cmake -S . -B build && ctest --timeout 30 --test-dir build --output-on-failure -R x",
		},
		{
			"/usr/bin/ctest --output-on-failure -R cli",
			"/usr/bin/ctest --timeout 30 --output-on-failure -R cli",
		},
		{
			"go test ./...",
			"go test ./...",
		},
	}
	for _, c := range cases {
		got := NormalizeCTestCommand(c.in)
		if got != c.want {
			t.Errorf("NormalizeCTestCommand(%q)=\n  %q\nwant %q", c.in, got, c.want)
		}
		// Idempotent.
		if again := NormalizeCTestCommand(got); again != got {
			t.Errorf("not idempotent: %q → %q", got, again)
		}
	}
}
