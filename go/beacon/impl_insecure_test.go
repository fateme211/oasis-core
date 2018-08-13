package beacon

import (
	"encoding/hex"
	"testing"

	"github.com/oasislabs/ekiden/go/epochtime/mock"

	"github.com/stretchr/testify/require"
)

func TestInsecureDummyRandomBeacon(t *testing.T) {
	const farFuture = 0xcafebabedeadbeef

	ts := mock.New()
	r := NewInsecureDummyRandomBeacon(ts)

	// Known Answer Tests taken from the Rust implementation, which
	// originally were generated by a throw-away Go snippet.

	b, err := r.GetBeacon(0)
	require.NoError(t, err, "GetBeacon(0)")
	require.Equal(
		t,
		"0c2d4edf3c57c2071f8856d1f74cb126455c2df949a2e3638509b20f8bd5e85d",
		hex.EncodeToString(b),
		"GetBeacon(0)",
	)

	b, err = r.GetBeacon(farFuture)
	require.NoError(t, err, "GetBeacon(0)")
	require.Equal(
		t,
		"36ae91d1c4c40e52bcaa86f5cbb8fe514f36e5165c721b18f5feabc25fb0aa84",
		hex.EncodeToString(b),
		"GetBeacon(0x%x)",
		uint64(farFuture),
	)
}
