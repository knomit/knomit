package fact

import (
	"testing"

	"github.com/dop251/goja"
	"github.com/stretchr/testify/require"
)

func TestGojaSmoke(t *testing.T) {
	vm := goja.New()
	v, err := vm.RunString("1 + 1")
	require.NoError(t, err)
	require.Equal(t, int64(2), v.ToInteger())
}
