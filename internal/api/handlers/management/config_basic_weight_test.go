package management

import "testing"

func TestNormalizeRoutingStrategyAliases(t *testing.T) {
	for want, inputs := range map[string][]string{
		"weighted-round-robin": {"weighted-round-robin", "weightedroundrobin", "wrr"},
		"quota-first":          {"quota-first", "Quota-First", "quotafirst"},
	} {
		for _, input := range inputs {
			got, ok := normalizeRoutingStrategy(input)
			if !ok || got != want {
				t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want %s, true", input, got, ok, want)
			}
		}
	}
}
