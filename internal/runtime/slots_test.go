package runtime

import "testing"

// TestSlots_NeverExceedsCapacity pins the non-blocking semaphore contract:
// exactly n acquisitions succeed, the next fails, and a release admits one
// more.
func TestSlots_NeverExceedsCapacity(t *testing.T) {
	t.Parallel()
	s := NewSlots(3)
	for i := 0; i < 3; i++ {
		if !s.TryAcquire() {
			t.Fatalf("acquisition %d failed below capacity", i+1)
		}
	}
	if s.TryAcquire() {
		t.Fatal("acquired a slot beyond capacity")
	}
	if s.InUse() != 3 || s.Cap() != 3 {
		t.Fatalf("InUse/Cap = %d/%d, want 3/3", s.InUse(), s.Cap())
	}
	s.Release()
	if !s.TryAcquire() {
		t.Fatal("released slot was not reusable")
	}
}

// TestSlots_DegenerateCapacityIsCoerced pins that a zero or negative
// configured cap cannot deadlock the provider: it becomes one slot.
func TestSlots_DegenerateCapacityIsCoerced(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, -5} {
		s := NewSlots(n)
		if s.Cap() != 1 {
			t.Fatalf("NewSlots(%d).Cap() = %d, want 1", n, s.Cap())
		}
		if !s.TryAcquire() {
			t.Fatalf("NewSlots(%d) admitted nothing", n)
		}
	}
}

// TestSlots_OverReleaseDoesNotPanic guards the request path: a double
// release must not take the process down.
func TestSlots_OverReleaseDoesNotPanic(t *testing.T) {
	t.Parallel()
	s := NewSlots(1)
	_ = s.TryAcquire()
	s.Release()
	s.Release() // must not panic or underflow
	if s.InUse() != 0 {
		t.Fatalf("InUse = %d after over-release, want 0", s.InUse())
	}
}
