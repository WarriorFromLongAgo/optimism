package derive

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/predeploys"
)

// L1ReceiptsFetcher fetches L1 header info and receipts for the payload attributes derivation (the info tx and deposits)
type L1ReceiptsFetcher interface {
	InfoByHash(ctx context.Context, hash common.Hash) (eth.BlockInfo, error)
	FetchReceipts(ctx context.Context, blockHash common.Hash) (eth.BlockInfo, types.Receipts, error)
}

type SystemConfigL2Fetcher interface {
	SystemConfigByL2Hash(ctx context.Context, hash common.Hash) (eth.SystemConfig, error)
}

// FetchingAttributesBuilder fetches inputs for the building of L2 payload attributes on the fly.
type FetchingAttributesBuilder struct {
	rollupCfg *rollup.Config
	l1        L1ReceiptsFetcher
	l2        SystemConfigL2Fetcher
}

func NewFetchingAttributesBuilder(rollupCfg *rollup.Config, l1 L1ReceiptsFetcher, l2 SystemConfigL2Fetcher) *FetchingAttributesBuilder {
	return &FetchingAttributesBuilder{
		rollupCfg: rollupCfg,
		l1:        l1,
		l2:        l2,
	}
}

// PreparePayloadAttributes prepares a PayloadAttributes template that is ready to build a L2 block with deposits only, on top of the given l2Parent, with the given epoch as L1 origin.
// The template defaults to NoTxPool=true, and no sequencer transactions: the caller has to modify the template to add transactions,
// by setting NoTxPool=false as sequencer, or by appending batch transactions as verifier.
// The severity of the error is returned; a crit=false error means there was a temporary issue, like a failed RPC or time-out.
// A crit=true error means the input arguments are inconsistent or invalid.
// PreparePayloadAttributes 准备一个 PayloadAttributes 模板，该模板已准备好在给定的 l2Parent 之上构建仅具有存款的 L2 块，并以给定的 epoch 作为 L1 原点。
// 模板默认为 NoTxPool=true，没有序列器交易：调用者必须修改模板以添加交易，
// 通过将 NoTxPool=false 设置为序列器，或通过附加批量交易作为验证者。
// 返回错误的严重性；crit=false 错误表示存在临时问题，例如 RPC 失败或超时。
// crit=true 错误表示输入参数不一致或无效。
// 主要功能是准备一个用于构建新的 L2 区块的 PayloadAttributes 模板。
// 这个方法处理了 L1 和 L2 之间的关系，确保了区块的时间一致性，并处理了网络升级和系统配置更新。它还负责创建必要的交易，如存款交易和 L1 信息交易。
// 确保了新的 L2 区块包含了正确的 L1 信息、系统配置更新、存款交易和其他必要的交易，同时维护了 L1 和 L2 之间的时间一致性。
func (ba *FetchingAttributesBuilder) PreparePayloadAttributes(ctx context.Context, l2Parent eth.L2BlockRef, epoch eth.BlockID) (attrs *eth.PayloadAttributes, err error) {
	var l1Info eth.BlockInfo
	var depositTxs []hexutil.Bytes
	var seqNumber uint64
	// 获取 L2 父区块的系统配置
	// 系统配置（SystemConfig）通常在 L1 新的 epoch 开始时更新，而不是在每个 L2 区块中。这是因为系统配置的变更通常与 L1 区块相关联，而不是 L2 区块。
	// 这些变更通常是通过 L1 上的特定合约调用触发的，例如更改 batcher 地址、调整 gas 限制等。
	// 在新的 epoch 开始时检查 SystemConfig 的变更是一种优化，因为它允许在处理新的 L1 信息时同时更新系统配置，而不需要在每个 L2 区块中都检查。
	// 这种设计平衡了更新频率和性能考虑，确保系统配置的变更能够及时反映在 L2 链上，同时避免了不必要的频繁检查。
	sysConfig, err := ba.l2.SystemConfigByL2Hash(ctx, l2Parent.Hash)
	if err != nil {
		return nil, NewTemporaryError(fmt.Errorf("failed to retrieve L2 parent block: %w", err))
	}

	// 这里的 epoch 并不是指 L1 的 32 个 slot。在 Optimism 的上下文中，epoch 指的是与单个 L1 区块相对应的 L2 区块序列。
	// 每当 L2 链开始处理新的 L1 区块信息时，就开始了一个新的 epoch。这可以在以下代码段中看到：
	// 如果 L1 的某个 slot 没有区块，L2 仍然会继续生成区块，但这些区块会基于上一个有效的 L1 区块信息。
	// L2 会继续每 2 秒生成一个新区块，即使 L1 没有新区块。
	// 这 6 个 L2 区块会使用相同的 L1 信息，即最后一个有效的 L1 区块信息。
	// 每个 L2 区块仍然会包含一个 L1 信息交易（l1InfoTx），但这些交易会包含相同的 L1 区块信息。
	// 序列号（SequenceNumber）会继续递增，表示在同一个 L1 区块内生成了多个 L2 区块。

	// If the L1 origin changed in this block, then we are in the first block of the epoch. In this
	// case we need to fetch all transaction receipts from the L1 origin block so we can scan for
	// user deposits.
	// 如果 L1 源在此块中发生变化，则我们处于该纪元的第一个块中。在这种情况下，我们需要从 L1 源块中获取所有交易收据，以便我们可以扫描用户存款。
	// 检查是否进入了新的 epoch（L1 源区块变化）：
	// l2Parent.L1Origin.Number 是当前 L2 区块的父区块所对应的 L1 区块号。
	// epoch.Number 是当前正在处理的 L1 区块号。
	// 如果这两个数字不相等，意味着 L2 链正在过渡到一个新的 L1 源区块。这种情况通常发生在：
	// L2 链刚刚完成了处理前一个 L1 区块中的所有信息。
	// 现在需要开始处理下一个 L1 区块的信息。
	// 因为当进入新的 epoch 时，需要执行一些特殊的操作，
	if l2Parent.L1Origin.Number != epoch.Number {
		// 如果是新 epoch，获取 L1 区块信息和收据
		// 主要作用获取 L1 区块的信息和交易收据。这个方法在新的 epoch 开始时被调用，
		info, receipts, err := ba.l1.FetchReceipts(ctx, epoch.Hash)
		if err != nil {
			return nil, NewTemporaryError(fmt.Errorf("failed to fetch L1 block info and receipts: %w", err))
		}
		// 验证 L1 原点的一致性
		if l2Parent.L1Origin.Hash != info.ParentHash() {
			return nil, NewResetError(
				fmt.Errorf("cannot create new block with L1 origin %s (parent %s) on top of L1 origin %s",
					epoch, info.ParentHash(), l2Parent.L1Origin))
		}
		// 从收据中派生存款交易
		// 从 L1 收据中提取并编码用户存款交易，以便在 L2 上处理
		// 这些是用户从 L1 到 L2 的存款交易。
		// 处理用户从 L1 向 L2 转移资产的请求
		// 确保 L1 上发起的存款在 L2 上得到正确处理
		deposits, err := DeriveDeposits(receipts, ba.rollupCfg.DepositContractAddress)
		if err != nil {
			// deposits may never be ignored. Failing to process them is a critical error.
			return nil, NewCriticalError(fmt.Errorf("failed to derive some deposits: %w", err))
		}
		// apply sysCfg changes
		// 使用 L1 收据更新系统配置
		// 从 L1 收据中提取系统配置更新信息，并应用这些更新
		if err := UpdateSystemConfigWithL1Receipts(&sysConfig, receipts, ba.rollupCfg, info.Time()); err != nil {
			return nil, NewCriticalError(fmt.Errorf("failed to apply derived L1 sysCfg updates: %w", err))
		}

		l1Info = info
		depositTxs = deposits
		seqNumber = 0
	} else {
		// 如果不是新 epoch，验证 L1 原点的一致性
		if l2Parent.L1Origin.Hash != epoch.Hash {
			return nil, NewResetError(fmt.Errorf("cannot create new block with L1 origin %s in conflict with L1 origin %s", epoch, l2Parent.L1Origin))
		}
		// 获取 L1 区块信息
		info, err := ba.l1.InfoByHash(ctx, epoch.Hash)
		if err != nil {
			return nil, NewTemporaryError(fmt.Errorf("failed to fetch L1 block info: %w", err))
		}
		l1Info = info
		depositTxs = nil
		seqNumber = l2Parent.SequenceNumber + 1
	}
	// 验证 L2 时间与 L1 时间的一致性
	// Sanity check the L1 origin was correctly selected to maintain the time invariant between L1 and L2
	// 健全性检查 L1 原点是否被正确选择以保持 L1 和 L2 之间的时间不变
	// 验证 L2 时间与 L1 时间的一致性是为了确保 L2 链（Layer 2）与 L1 链（Layer 1）之间保持正确的时间关系。
	// 时间同步：确保 L2 区块的时间戳不会早于其对应的 L1 原点区块的时间戳。这维护了 L1 和 L2 之间的因果关系。
	// 防止回滚攻击：通过确保 L2 时间不会落后于 L1 时间，可以防止潜在的回滚攻击或时间相关的漏洞。
	// 保持一致性：这种检查有助于维护整个系统的一致性，确保 L2 链正确地跟随 L1 链的进展。
	// 优化性能：通过保持适当的时间关系，可以优化跨层交互和状态同步。
	// 符合协议规则：这种时间验证是 Optimistic Rollup 协议的一个重要部分，确保系统按照预期的方式运行。
	// 这段代码检查下一个 L2 区块的时间是否不早于其 L1 原点区块的时间。如果违反了这个条件，就会返回一个重置错误，表明需要重新同步或调整区块生成过程。
	nextL2Time := l2Parent.Time + ba.rollupCfg.BlockTime
	if nextL2Time < l1Info.Time() {
		return nil, NewResetError(fmt.Errorf("cannot build L2 block on top %s for time %d before L1 origin %s at time %d",
			l2Parent, nextL2Time, eth.ToBlockID(l1Info), l1Info.Time()))
	}
	// 处理网络升级交易
	// 这些是网络升级交易，包括 Ecotone 和 Fjord 升级。它们的作用是：
	// 在特定时间点激活新的网络功能,确保网络平滑升级,可能包含必要的状态迁移或配置更改
	// 这些不同类型的交易被组合在一起，形成了 L2 区块的交易列表。这种组合确保了 L2 链能够正确地跟踪 L1 状态、处理跨链操作、执行网络升级，并维护整个系统的一致性和安全性。
	var upgradeTxs []hexutil.Bytes
	// 检查是否是 Ecotone 激活区块，如果是，生成相应的升级交易。
	if ba.rollupCfg.IsEcotoneActivationBlock(nextL2Time) {
		upgradeTxs, err = EcotoneNetworkUpgradeTransactions()
		if err != nil {
			return nil, NewCriticalError(fmt.Errorf("failed to build ecotone network upgrade txs: %w", err))
		}
	}
	// 检查是否是 Fjord 激活区块，如果是，生成相应的升级交易。
	if ba.rollupCfg.IsFjordActivationBlock(nextL2Time) {
		fjord, err := FjordNetworkUpgradeTransactions()
		if err != nil {
			return nil, NewCriticalError(fmt.Errorf("failed to build fjord network upgrade txs: %w", err))
		}
		upgradeTxs = append(upgradeTxs, fjord...)
	}
	// 创建 L1 信息交易
	// 生成包含 L1 信息的存款交易。
	// L1InfoDepositBytes 函数生成包含 L1 信息的存款交易的主要原因是：
	// 同步 L1 和 L2 状态：这个交易将 L1 的关键信息传递到 L2，确保 L2 链能够跟踪和验证 L1 的状态。
	// 提供 L1 数据可用性：L2 上的智能合约和用户可能需要访问 L1 的信息，如区块号、时间戳等。
	// 安全性：通过在每个 L2 区块中包含 L1 信息，可以防止某些类型的攻击，并增强 L2 链的安全性。
	// 费用计算：L1 的信息（如 gas 价格）可能用于计算 L2 上的交易费用。
	// 跨层交互：这些信息对于处理跨层交互（如存款和取款）至关重要。
	// 协议规则：这是 Optimistic Rollup 协议的一个关键部分，确保 L2 链正确地遵循 L1 链。
	//  主要指的是从 L1 区块信息生成的存款交易数据。这些数据包含了 L1 区块的关键信息，用于在 L2 链上创建一个特殊的交易，以同步 L1 和 L2 之间的状态。
	l1InfoTx, err := L1InfoDepositBytes(ba.rollupCfg, sysConfig, seqNumber, l1Info, nextL2Time)
	if err != nil {
		return nil, NewCriticalError(fmt.Errorf("failed to create l1InfoTx: %w", err))
	}
	// 处理特殊的 interop 交易
	var afterForceIncludeTxs []hexutil.Bytes
	// 这些是特殊的 interop 交易，主要是 DepositsComplete 交易。它的作用是：
	// 标记所有存款交易已完成处理
	// 在 interop 时期使用，用于确保跨链操作的完整性
	if ba.rollupCfg.IsInterop(nextL2Time) {
		// 如果是 interop 时期，创建 DepositsComplete 交易。
		// 用于标记所有存款交易已完成处理的特殊交易。这个函数在以下位置定义：
		depositsCompleteTx, err := DepositsCompleteBytes(seqNumber, l1Info)
		if err != nil {
			return nil, NewCriticalError(fmt.Errorf("failed to create depositsCompleteTx: %w", err))
		}
		afterForceIncludeTxs = append(afterForceIncludeTxs, depositsCompleteTx)
	}
	// 组合所有交易
	// 每当创建新的 L2 区块时，都会生成一个 L1InfoTx。
	// 在验证 L2 区块时，也会检查每个区块是否包含 L1 信息：
	// 每个 L2 区块都包含一个 l1InfoTx，即使在一个 L1 区块时间内生成了多个 L2 区块。这是为了确保每个 L2 区块都能够追踪其对应的 L1 状态，即使这个状态在连续的几个 L2 区块中没有变化。
	// 每个 L2 区块都包含最新的 L1 信息，保持了整个系统的一致性。
	txs := make([]hexutil.Bytes, 0, 1+len(depositTxs)+len(afterForceIncludeTxs)+len(upgradeTxs))
	txs = append(txs, l1InfoTx)
	txs = append(txs, depositTxs...)
	txs = append(txs, afterForceIncludeTxs...)
	txs = append(txs, upgradeTxs...)
	// 处理提款和信标链根
	var withdrawals *types.Withdrawals
	if ba.rollupCfg.IsCanyon(nextL2Time) {
		withdrawals = &types.Withdrawals{}
	}
	var parentBeaconRoot *common.Hash
	if ba.rollupCfg.IsEcotone(nextL2Time) {
		parentBeaconRoot = l1Info.ParentBeaconRoot()
		if parentBeaconRoot == nil { // default to zero hash if there is no beacon-block-root available
			parentBeaconRoot = new(common.Hash)
		}
	}
	// 返回准备好的 PayloadAttributes
	return &eth.PayloadAttributes{
		Timestamp:             hexutil.Uint64(nextL2Time),
		PrevRandao:            eth.Bytes32(l1Info.MixDigest()),
		SuggestedFeeRecipient: predeploys.SequencerFeeVaultAddr,
		Transactions:          txs,
		NoTxPool:              true,
		GasLimit:              (*eth.Uint64Quantity)(&sysConfig.GasLimit),
		Withdrawals:           withdrawals,
		ParentBeaconBlockRoot: parentBeaconRoot,
	}, nil
}
