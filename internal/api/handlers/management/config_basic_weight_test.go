package management

import "testing"

func TestNormalizeRoutingStrategyWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"weighted-round-robin", "weightedroundrobin", "wrr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want weighted-round-robin, true", input, got, ok)
		}
	}
}

func TestNormalizeRoutingStrategyQuotaResetFirst(t *testing.T) {
	for _, input := range []string{"quota-reset-first", "quotaresetfirst", "reset-first", "qrf"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "quota-reset-first" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want quota-reset-first, true", input, got, ok)
		}
	}
}
