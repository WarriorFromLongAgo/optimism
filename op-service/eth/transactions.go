package eth

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

type L1Client interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	NonceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (uint64, error)
}

// EncodeTransactions encodes a list of transactions into opaque transactions.
func EncodeTransactions(elems []*types.Transaction) ([]hexutil.Bytes, error) {
	out := make([]hexutil.Bytes, len(elems))
	for i, el := range elems {
		dat, err := el.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("failed to marshal tx %d: %w", i, err)
		}
		out[i] = dat
	}
	return out, nil
}

// DecodeTransactions decodes a list of opaque transactions into transactions.
func DecodeTransactions(data []hexutil.Bytes) ([]*types.Transaction, error) {
	dest := make([]*types.Transaction, len(data))
	for i := range dest {
		var x types.Transaction
		if err := x.UnmarshalBinary(data[i]); err != nil {
			return nil, fmt.Errorf("failed to unmarshal tx %d: %w", i, err)
		}
		dest[i] = &x
	}
	return dest, nil
}

// TransactionsToHashes computes the transaction-hash for every transaction in the input.
func TransactionsToHashes(elems []*types.Transaction) []common.Hash {
	out := make([]common.Hash, len(elems))
	for i, el := range elems {
		out[i] = el.Hash()
	}
	return out
}

// CheckRecentTxs checks the depth recent blocks for txs from the account with address addr
// and returns either:
//   - blockNum containing the last tx and true if any was found
//   - the oldest block checked and false if no nonce change was found
//
// CheckRecentTxs 检查地址为 addr 的账户的近期区块深度
// 并返回：
// - blockNum 包含最后一个 tx，如果发现任何 tx，则返回 true
// - 检查最旧的区块，如果未发现 nonce 更改，则返回 false
func CheckRecentTxs(
	ctx context.Context,
	// L1 客户端接口，提供与 L1 区块链交互的方法。
	l1 L1Client,
	// 要检查的区块深度。
	depth int,
	// 要检查的账户地址。
	addr common.Address,
) (blockNum uint64, found bool, err error) {
	// blockNum 包含最后一个交易的区块号。
	// found bool: 指示是否找到交易。

	// 检查指定账户在最近的区块中是否有交易，并返回包含最后一个交易的区块号和一个布尔值，指示是否找到交易。

	// 调用 l1.HeaderByNumber 方法获取当前区块头。
	blockHeader, err := l1.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("failed to retrieve current block header: %w", err)
	}

	// 获取当前区块号。
	currentBlock := blockHeader.Number
	// 调用 l1.NonceAt 方法获取账户在当前区块的 nonce。
	currentNonce, err := l1.NonceAt(ctx, addr, currentBlock)
	if err != nil {
		return 0, false, fmt.Errorf("failed to retrieve current nonce: %w", err)
	}

	// 计算最旧区块号。
	oldestBlock := new(big.Int).Sub(currentBlock, big.NewInt(int64(depth)))
	// 调用 l1.NonceAt 方法获取账户在最旧区块的 nonce。
	previousNonce, err := l1.NonceAt(ctx, addr, oldestBlock)
	if err != nil {
		return 0, false, fmt.Errorf("failed to retrieve previous nonce: %w", err)
	}

	// 如果当前 nonce 和最旧 nonce 相同，表示最近的交易早于给定的深度，返回最旧区块号和 false。
	if currentNonce == previousNonce {
		// Most recent tx is older than the given depth
		return oldestBlock.Uint64(), false, nil
	}

	// Use binary search to find the block where the nonce changed
	low := oldestBlock.Uint64()
	high := currentBlock.Uint64()

	// 使用二分查找算法找到 nonce 变化的区块。
	for low < high {
		mid := (low + high) / 2
		midNonce, err := l1.NonceAt(ctx, addr, new(big.Int).SetUint64(mid))
		if err != nil {
			return 0, false, fmt.Errorf("failed to retrieve nonce at block %d: %w", mid, err)
		}

		if midNonce > currentNonce {
			// Catch a reorg that causes inconsistent nonce
			return CheckRecentTxs(ctx, l1, depth, addr)
		} else if midNonce == currentNonce {
			high = mid
		} else {
			// midNonce < currentNonce: check the next block to see if we've found the
			// spot where the nonce transitions to the currentNonce
			nextBlockNum := mid + 1
			nextBlockNonce, err := l1.NonceAt(ctx, addr, new(big.Int).SetUint64(nextBlockNum))
			if err != nil {
				return 0, false, fmt.Errorf("failed to retrieve nonce at block %d: %w", mid, err)
			}

			// 如果找到 nonce 变化的区块，返回该区块号和 true。
			if nextBlockNonce == currentNonce {
				return nextBlockNum, true, nil
			}
			low = mid + 1
		}
	}

	// 如果未找到，返回最旧区块号和 false。
	return oldestBlock.Uint64(), false, nil
}
