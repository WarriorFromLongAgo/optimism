package vm

import (
	"math/big"
	"slices"
	"testing"

	"github.com/ethereum-optimism/optimism/op-challenger/game/fault/trace/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestOpProgramFillHostCommand(t *testing.T) {
	dir := "mockdir"
	cfg := Config{
		L1:       "http://localhost:8888",
		L1Beacon: "http://localhost:9000",
		L2:       "http://localhost:9999",
		Server:   "./bin/mockserver",
	}
	inputs := utils.LocalGameInputs{
		L1Head:        common.Hash{0x11},
		L2Head:        common.Hash{0x22},
		L2OutputRoot:  common.Hash{0x33},
		L2Claim:       common.Hash{0x44},
		L2BlockNumber: big.NewInt(3333),
	}
	vmArgs := NewOpProgramVmArgs(cfg, &inputs)

	args := []string{}
	err := vmArgs.FillHostCommand(&args, dir)
	require.NoError(t, err)

	require.True(t, slices.Contains(args, "--"))
	require.True(t, slices.Contains(args, "--server"))
	require.True(t, slices.Contains(args, "--l1"))
	require.True(t, slices.Contains(args, "--l1.beacon"))
	require.True(t, slices.Contains(args, "--l2"))
	require.True(t, slices.Contains(args, "--datadir"))
	require.True(t, slices.Contains(args, "--l1.head"))
	require.True(t, slices.Contains(args, "--l2.head"))
	require.True(t, slices.Contains(args, "--l2.outputroot"))
	require.True(t, slices.Contains(args, "--l2.claim"))
	require.True(t, slices.Contains(args, "--l2.blocknumber"))
}