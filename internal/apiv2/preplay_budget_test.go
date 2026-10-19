package apiv2

import (
	"context"
	"testing"
	"time"
)

// The catalog leaf detail wait prices itself against the deployment's
// configured budget; every failure mode (unwired reader, unreadable setting,
// non-positive answer) falls back to the built-in default rather than
// stretching the wait.
func TestPreplayDiscoveryCatalogBudgetForFallsBackToDefault(t *testing.T) {
	ctx := context.Background()

	if got := (&Registry{}).preplayDiscoveryCatalogBudgetFor(ctx); got != preplayDiscoveryCatalogBudget {
		t.Fatalf("unwired budget reader = %s, want the built-in default %s", got, preplayDiscoveryCatalogBudget)
	}

	for _, test := range []struct {
		name   string
		want   time.Duration
		reader func(context.Context) int
	}{
		{name: "configured default", want: 3 * time.Second, reader: func(context.Context) int { return 3000 }},
		{name: "configured custom", want: 900 * time.Millisecond, reader: func(context.Context) int { return 900 }},
		{name: "non-positive answer", want: preplayDiscoveryCatalogBudget, reader: func(context.Context) int { return -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			reg := &Registry{deps: Dependencies{PreplayDiscoveryBudgetMs: test.reader}}
			if got := reg.preplayDiscoveryCatalogBudgetFor(ctx); got != test.want {
				t.Fatalf("budget = %s, want %s", got, test.want)
			}
		})
	}
}
