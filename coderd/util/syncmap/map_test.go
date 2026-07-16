package syncmap_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/coder/coder/v2/coderd/util/syncmap"
)

func TestMapLoadOrStore(t *testing.T) {
	t.Parallel()

	m := syncmap.New[string, *atomic.Int64]()

	// First store: returns the value we passed, loaded=false.
	v, loaded := m.LoadOrStore("a", atomic.NewInt64(1))
	require.False(t, loaded)
	require.Equal(t, int64(1), v.Load())

	// Second call with same key: returns the existing value, loaded=true.
	v, loaded = m.LoadOrStore("a", atomic.NewInt64(2))
	require.True(t, loaded)
	require.Equal(t, int64(1), v.Load(), "should return the existing value, not the new one")
}

func TestMapLoad(t *testing.T) {
	t.Parallel()

	m := syncmap.New[string, int]()
	m.Store("x", 42)

	v, ok := m.Load("x")
	require.True(t, ok)
	require.Equal(t, 42, v)

	_, ok = m.Load("missing")
	require.False(t, ok)
}

func TestMapLoadAndDelete(t *testing.T) {
	t.Parallel()

	m := syncmap.New[string, int]()
	m.Store("x", 42)

	v, loaded := m.LoadAndDelete("x")
	require.True(t, loaded)
	require.Equal(t, 42, v)

	_, loaded = m.LoadAndDelete("x")
	require.False(t, loaded)
}
