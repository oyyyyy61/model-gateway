package routing

import "testing"

func TestPickerSkipsUnhealthyAndReleasesInflight(t *testing.T) {
	picker := NewPicker([]Instance{{ID: "one", Model: "demo", Endpoint: "http://one", Weight: 1, MaxInflight: 1}, {ID: "two", Model: "demo", Endpoint: "http://two", Weight: 1, MaxInflight: 1}})
	picker.SetHealth("one", false)
	instance, release, ok := picker.Acquire("demo")
	if !ok || instance.ID != "two" {
		t.Fatalf("expected healthy instance two, got %#v, %v", instance, ok)
	}
	if _, _, ok := picker.Acquire("demo"); ok {
		t.Fatal("expected inflight limit to reject a second request")
	}
	release()
	if _, _, ok := picker.Acquire("demo"); !ok {
		t.Fatal("expected capacity after release")
	}
}
