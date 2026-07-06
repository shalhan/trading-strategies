package indicator

import (
	"math"
	"testing"
)

func TestEMASeedThenRecurrence(t *testing.T) {
	e := NewEMA(3) // multiplier = 2/4 = 0.5

	if _, ready := e.Update(10); ready {
		t.Fatal("ready after 1, want not ready")
	}
	if _, ready := e.Update(20); ready {
		t.Fatal("ready after 2, want not ready")
	}
	v, ready := e.Update(30) // seed = mean(10,20,30) = 20
	if !ready || math.Abs(v-20) > 1e-9 {
		t.Fatalf("seed EMA=%v ready=%v, want 20 true", v, ready)
	}
	// next: 0.5*40 + 0.5*20 = 30
	v2, _ := e.Update(40)
	if math.Abs(v2-30) > 1e-9 {
		t.Errorf("EMA=%v, want 30", v2)
	}
}
