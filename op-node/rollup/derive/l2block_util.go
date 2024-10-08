package derive

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// L2BlockRefSource is a source for the generation of a L2BlockRef. E.g. a
// *types.Block is a L2BlockRefSource.
//
// L2BlockToBlockRef extracts L2BlockRef from a L2BlockRefSource. The first
// transaction of a source must be a Deposit transaction.
type L2BlockRefSource interface {
	Hash() common.Hash
	ParentHash() common.Hash
	NumberU64() uint64
	Time() uint64
	Transactions() types.Transactions
}

// L2BlockToBlockRef extracts the essential L2BlockRef information from an L2
// block ref source, falling back to genesis information if necessary.
// L2BlockToBlockRef 从 L2 区块源提取必要的 L2BlockRef 信息
// 如果是创世区块,则使用创世信息;否则,从区块的第一个交易(必须是存款交易)中提取 L1 信息
// 主要功能是从给定的 L2 区块源提取必要的信息,构造一个 L2BlockRef 结构
func L2BlockToBlockRef(rollupCfg *rollup.Config, block L2BlockRefSource) (eth.L2BlockRef, error) {
	// 1. 获取区块的哈希和编号
	hash, number := block.Hash(), block.NumberU64()

	var l1Origin eth.BlockID
	var sequenceNumber uint64
	genesis := &rollupCfg.Genesis

	// 2. 检查是否为创世区块
	if number == genesis.L2.Number {
		// 2.1 如果是创世区块,验证哈希是否匹配
		if hash != genesis.L2.Hash {
			return eth.L2BlockRef{}, fmt.Errorf("expected L2 genesis hash to match L2 block at genesis block number %d: %s <> %s", genesis.L2.Number, hash, genesis.L2.Hash)
		}
		// 2.2 使用创世配置中的 L1 信息
		l1Origin = genesis.L1
		sequenceNumber = 0
	} else {
		// 3. 非创世区块,从第一个交易提取 L1 信息
		txs := block.Transactions()
		// 3.1 检查区块是否有交易
		if txs.Len() == 0 {
			return eth.L2BlockRef{}, fmt.Errorf("l2 block is missing L1 info deposit tx, block hash: %s", hash)
		}
		// 3.2 检查第一个交易是否为存款交易
		tx := txs[0]
		if tx.Type() != types.DepositTxType {
			return eth.L2BlockRef{}, fmt.Errorf("first payload tx has unexpected tx type: %d", tx.Type())
		}
		// 3.3 从交易数据中解析 L1 信息
		info, err := L1BlockInfoFromBytes(rollupCfg, block.Time(), tx.Data())
		if err != nil {
			return eth.L2BlockRef{}, fmt.Errorf("failed to parse L1 info deposit tx from L2 block: %w", err)
		}
		// 3.4 设置 L1 来源和序列号
		l1Origin = eth.BlockID{Hash: info.BlockHash, Number: info.Number}
		sequenceNumber = info.SequenceNumber
	}

	// 4. 构造并返回 L2BlockRef
	return eth.L2BlockRef{
		Hash:           hash,
		Number:         number,
		ParentHash:     block.ParentHash(),
		Time:           block.Time(),
		L1Origin:       l1Origin,
		SequenceNumber: sequenceNumber,
	}, nil
}
