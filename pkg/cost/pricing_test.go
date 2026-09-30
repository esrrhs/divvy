package cost

import "testing"

func TestPriceCost(t *testing.T) {
	p := Price{InputPerM: 1.0, OutputPerM: 3.0}
	got := p.Cost(1_000_000, 2_000_000)
	if got != 7.0 {
		t.Fatalf("cost = %v, want 7.0", got)
	}
}

func TestDefaultPricing(t *testing.T) {
	p := DefaultPricing()
	cases := map[string]bool{
		"gpt-4o-mini":       true,
		"GPT-4o-Mini":       true, // case-insensitive exact
		"gpt-4o-mini-2024":  true, // suffix via substring
		"qwen2.5-coder:14b": false,
		"":                  false,
	}
	for model, want := range cases {
		if _, ok := p.PriceFor(model); ok != want {
			t.Fatalf("PriceFor(%q) ok=%v, want %v", model, ok, want)
		}
	}
}

func TestPricingMostSpecificSubstring(t *testing.T) {
	// "gpt-4o-mini" is longer than "gpt-4o" and must win for this model.
	p := DefaultPricing()
	price, ok := p.PriceFor("gpt-4o-mini-2024-07-18")
	if !ok {
		t.Fatal("no price")
	}
	if price != (Price{InputPerM: 0.15, OutputPerM: 0.60}) {
		t.Fatalf("got %+v", price)
	}
}

func TestParseJSON(t *testing.T) {
	spec := `{
		"my-model": {"input": 0.25, "output": 0.75},
		"free-local": {"input": 0, "output": 0}
	}`
	p, err := ParseJSON(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := p.PriceFor("my-model")
	if !ok || got.Cost(1_000_000, 1_000_000) != 1.0 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	zero, ok := p.PriceFor("free-local")
	if !ok || !zero.Zero() {
		t.Fatalf("got %+v ok=%v", zero, ok)
	}
}

func TestParseJSONErrors(t *testing.T) {
	for _, bad := range []string{
		``,
		`{}`,
		`{"": {"input":1,"output":1}}`,
		`{"x":{"input":-1,"output":1}}`,
		`not json`,
	} {
		if _, err := ParseJSON(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestMergeOverridesDefault(t *testing.T) {
	merged := DefaultPricing().Merge(mustBuild(map[string]Price{
		"gpt-4o-mini": {InputPerM: 9.0, OutputPerM: 9.0},
		"custom-xyz":  {InputPerM: 0.01, OutputPerM: 0.02},
	}))
	if got, _ := merged.PriceFor("gpt-4o-mini"); got.InputPerM != 9.0 {
		t.Fatalf("override failed: %+v", got)
	}
	if _, ok := merged.PriceFor("custom-xyz"); !ok {
		t.Fatal("custom entry missing")
	}
	if _, ok := merged.PriceFor("gpt-4.1"); !ok {
		t.Fatal("built-in entry lost")
	}
}

func TestFormatUSD(t *testing.T) {
	cases := map[float64]string{
		0.000123: "$0.000123",
		0.05:     "$0.0500",
		2.5:      "$2.50",
	}
	for in, want := range cases {
		if got := FormatUSD(in); got != want {
			t.Fatalf("FormatUSD(%v) = %q, want %q", in, got, want)
		}
	}
}
