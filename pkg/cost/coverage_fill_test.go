package cost

import "testing"

// TestPriceForNilReceiver covers the nil-safe lookup: a nil *Pricing reports
// "no price" instead of panicking.
func TestPriceForNilReceiver(t *testing.T) {
	var p *Pricing
	if _, ok := p.PriceFor("gpt-4o"); ok {
		t.Fatal("nil Pricing must not report a price")
	}
}

// TestMergeWithNilIsIdentity covers Merge with a nil overlay: the receiver is
// returned unchanged.
func TestMergeWithNilIsIdentity(t *testing.T) {
	p := DefaultPricing()
	if got := p.Merge(nil); got != p {
		t.Fatal("Merge(nil) must return the receiver")
	}
}
