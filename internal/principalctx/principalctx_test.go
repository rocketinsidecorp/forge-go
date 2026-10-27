package principalctx_test

import (
	"context"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/principalctx"
)

func TestValueReturnsWhatWithStored(t *testing.T) {
	t.Parallel()
	ctx := principalctx.With(context.Background(), "someone")
	if got := principalctx.Value(ctx); got != "someone" {
		t.Errorf("Value = %v, want the stored principal", got)
	}
	if got := principalctx.Value(context.Background()); got != nil {
		t.Errorf("Value of an empty context = %v, want nil", got)
	}
	var none context.Context
	if got := principalctx.Value(none); got != nil {
		t.Errorf("Value of a nil context = %v, want nil", got)
	}
}
