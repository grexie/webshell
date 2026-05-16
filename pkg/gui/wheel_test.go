package gui

import "testing"

func TestWheelSteps(t *testing.T) {
	tests := []struct {
		name  string
		delta float64
		want  int
	}{
		{name: "zero", delta: 0, want: 0},
		{name: "noise", delta: 0.2, want: 0},
		{name: "small positive", delta: 12, want: 1},
		{name: "small negative", delta: -12, want: 1},
		{name: "coalesced", delta: 240, want: 3},
		{name: "clamped", delta: 2000, want: 8},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := wheelSteps(test.delta); got != test.want {
				t.Fatalf("wheelSteps(%v) = %d, want %d", test.delta, got, test.want)
			}
		})
	}
}
