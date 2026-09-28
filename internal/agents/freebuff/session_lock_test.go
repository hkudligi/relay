package freebuff

import "testing"

func TestSessionLockIsExclusive(t *testing.T) {
	first, err := acquireSessionLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()

	second, err := acquireSessionLock()
	if err == nil {
		second.release()
		t.Fatal("second Freebuff session acquired the lock")
	}
	if err != ErrSessionConflict {
		t.Fatalf("second lock error = %v, want %v", err, ErrSessionConflict)
	}
}
