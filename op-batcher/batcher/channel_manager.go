package batcher

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ethereum-optimism/optimism/op-batcher/metrics"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

var ErrReorg = errors.New("block does not extend existing chain")

// channelManager stores a contiguous set of blocks & turns them into channels.
// 存储一组连续的块并将它们转换为通道。
// Upon receiving tx confirmation (or a tx failure), it does channel error handling.
// 收到 tx 确认（或 tx 失败）后，它会进行通道错误处理。
//
// For simplicity, it only creates a single pending channel at a time & waits for
// 为简单起见，它一次只创建一个待处理的通道，并等待
// the channel to either successfully be submitted or timeout before creating a new
// channel.
// 通道成功提交或超时后再创建新的通道。
// Public functions on channelManager are safe for concurrent access.
// channelManager 上的公共函数对于并发访问是安全的。
type channelManager struct {
	// 互斥锁，用于保护并发访问。
	mu sync.Mutex
	// 日志记录器。
	log log.Logger
	// Metricer 指标记录器。
	metr metrics.Metricer
	// 通道配置提供者。
	cfgProvider ChannelConfigProvider
	// Rollup 配置。
	rollupCfg *rollup.Config

	// All blocks since the last request for new tx data.
	// 存储自上次请求以来的所有区块。
	blocks []*types.Block
	// The latest L1 block from all the L2 blocks in the most recently closed channel
	// 最近关闭的通道中的最新 L1 区块。
	l1OriginLastClosedChannel eth.BlockID
	// The default ChannelConfig to use for the next channel
	// 默认的通道配置。
	defaultCfg ChannelConfig
	// last block hash - for reorg detection
	// 最后一个区块的哈希，用于检测重组。
	tip common.Hash

	// channel to write new block data to
	// 当前正在写入新区块数据的通道。
	currentChannel *channel
	// channels to read frame data from, for writing batches onchain
	// 通道队列，用于从中读取帧数据并写入链上。
	channelQueue []*channel
	// used to lookup channels by tx ID upon tx success / failure
	// 用于根据交易 ID 查找通道。
	txChannels map[string]*channel

	// if set to true, prevents production of any new channel frames
	// 标志是否已关闭，防止生成新的通道帧。
	closed bool
}

// NewChannelManager 创建并返回一个新的 channelManager 实例。
func NewChannelManager(log log.Logger, metr metrics.Metricer, cfgProvider ChannelConfigProvider, rollupCfg *rollup.Config) *channelManager {
	return &channelManager{
		log:         log,
		metr:        metr,
		cfgProvider: cfgProvider,
		defaultCfg:  cfgProvider.ChannelConfig(),
		rollupCfg:   rollupCfg,
		txChannels:  make(map[string]*channel),
	}
}

// Clear clears the entire state of the channel manager.
// It is intended to be used before launching op-batcher and after an L2 reorg.
// 清除通道管理器的整个状态，通常在启动 op-batcher 和 L2 重组后使用。
func (s *channelManager) Clear(l1OriginLastClosedChannel eth.BlockID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.Trace("clearing channel manager state")
	s.blocks = s.blocks[:0]
	s.l1OriginLastClosedChannel = l1OriginLastClosedChannel
	s.tip = common.Hash{}
	s.closed = false
	s.currentChannel = nil
	s.channelQueue = nil
	s.txChannels = make(map[string]*channel)
}

// TxFailed records a transaction as failed. It will attempt to resubmit the data
// in the failed transaction.
// TxFailed 将交易记录为失败。它将尝试重新提交失败交易中的数据。
// TxFailed 记录一个交易失败。它会尝试重新提交失败交易中的数据。
func (s *channelManager) TxFailed(_id txID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 将 txID 转换为字符串形式
	id := _id.String()
	// 检查是否存在与该交易 ID 关联的通道
	if channel, ok := s.txChannels[id]; ok {
		// 从 txChannels 映射中删除该交易 ID
		delete(s.txChannels, id)
		// 调用通道的 TxFailed 方法,处理失败的交易
		channel.TxFailed(id)
		// 如果 channelManager 已关闭且该通道没有已提交的交易
		if s.closed && channel.NoneSubmitted() {
			// 记录日志,表示将清除该通道以进行关闭
			s.log.Info("Channel has no submitted transactions, clearing for shutdown", "chID", channel.ID())
			// 从待处理通道列表中移除该通道
			s.removePendingChannel(channel)
		}
	} else {
		s.log.Warn("transaction from unknown channel marked as failed", "id", id)
	}
}

// TxConfirmed marks a transaction as confirmed on L1. Unfortunately even if all frames in
// a channel have been marked as confirmed on L1 the channel may be invalid & need to be
// resubmitted.
// This function may reset the pending channel if the pending channel has timed out.
// TxConfirmed 将交易标记为在 L1 上已确认。不幸的是，即使通道中的所有帧
// 都已在 L1 上标记为已确认，该通道也可能无效且需要重新提交。
// 如果待处理通道已超时，则此功能可能会重置待处理通道。
// TxConfirmed 将交易标记为在 L1 上已确认。
// 即使通道中的所有帧都已在 L1 上标记为已确认，该通道也可能无效且需要重新提交。
// 如果待处理通道已超时，则此函数可能会重置待处理通道。
func (s *channelManager) TxConfirmed(_id txID, inclusionBlock eth.BlockID) {
	// 加锁以确保并发安全
	s.mu.Lock()
	defer s.mu.Unlock()
	// 将 txID 转换为字符串形式
	id := _id.String()
	// 检查是否存在与该交易 ID 关联的通道
	if channel, ok := s.txChannels[id]; ok {
		// 从 txChannels 映射中删除该交易 ID
		delete(s.txChannels, id)
		// 调用通道的 TxConfirmed 方法,处理已确认的交易
		// done 表示通道是否已完成,blocks 是可能需要重新排队的区块
		done, blocks := channel.TxConfirmed(id, inclusionBlock)
		// 将可能需要重新处理的区块添加到区块队列的前面
		s.blocks = append(blocks, s.blocks...)
		if done {
			// 如果通道已完成,从待处理通道列表中移除
			s.removePendingChannel(channel)
		}
	} else {
		// 如果找不到与该交易 ID 关联的通道,记录警告日志
		s.log.Warn("transaction from unknown channel marked as confirmed", "id", id)
	}
	// 记录批次交易提交的指标
	s.metr.RecordBatchTxSubmitted()
	s.log.Debug("marked transaction as confirmed", "id", id, "block", inclusionBlock)
}

// removePendingChannel removes the given completed channel from the manager's state.
// removePendingChannel 从管理器状态中删除给定的完成通道。
func (s *channelManager) removePendingChannel(channel *channel) {
	// 1. 检查并清除当前通道
	// 如果给定的通道是当前正在处理的通道,将其设置为 nil
	if s.currentChannel == channel {
		s.currentChannel = nil
	}
	// 2. 在通道队列中查找给定通道的索引
	index := -1
	for i, c := range s.channelQueue {
		if c == channel {
			index = i
			break
		}
	}
	// 3. 如果在队列中找不到该通道,记录警告并返回
	if index < 0 {
		s.log.Warn("channel not found in channel queue", "id", channel.ID())
		return
	}
	// 4. 从通道队列中移除该通道
	// 使用切片操作,将索引前后的元素拼接起来,effectively 删除该索引处的通道
	s.channelQueue = append(s.channelQueue[:index], s.channelQueue[index+1:]...)
}

// nextTxData dequeues frames from the channel and returns them encoded in a transaction.
// It also updates the internal tx -> channels mapping
// 从通道中出队帧，并以交易的形式返回它们。它还会更新内部 tx -> 通道映射
func (s *channelManager) nextTxData(channel *channel) (txData, error) {
	if channel == nil || !channel.HasTxData() {
		s.log.Trace("no next tx data")
		return txData{}, io.EOF // TODO: not enough data error instead
	}
	// 从通道中获取下一个待发送的交易数据
	tx := channel.NextTxData()
	// 将交易 ID 与通道关联，用于后续的确认处理
	s.txChannels[tx.ID().String()] = channel
	return tx, nil
}

// TxData returns the next tx data that should be submitted to L1.
// TxData 返回下一个应提交给 L1 的 tx 数据。
//
// If the current channel is
// full, it only returns the remaining frames of this channel until it got
// successfully fully sent to L1. It returns io.EOF if there's no pending tx data.
// 如果当前通道已满，则仅返回此通道的剩余帧，直到成功完全发送到 L1。如果没有待处理的 tx 数据，则返回 io.EOF。
//
// It will decide whether to switch DA type automatically.
// 它将决定是否自动切换 DA 类型。
// When switching DA type, the channelManager state will be rebuilt
// with a new ChannelConfig.
// 切换 DA 类型时，将使用新的 ChannelConfig 重建 channelManager 状态。
func (s *channelManager) TxData(l1Head eth.BlockID) (txData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 2. 获取准备好的通道
	// 获取一个准备好的通道，这可能涉及处理待处理的区块
	channel, err := s.getReadyChannel(l1Head)
	if err != nil {
		return emptyTxData, err
	}
	// If the channel has already started being submitted,
	// return now and ensure no requeueing happens
	// 3. 检查通道是否已开始提交
	// 如果通道已经开始提交，直接返回下一个交易数据，不进行重新排队
	if !channel.NoneSubmitted() {
		return s.nextTxData(channel)
	}

	// Call provider method to reassess optimal DA type
	// 4. 重新评估最优 DA 类型
	// 调用提供者方法重新评估最优 DA 类型
	newCfg := s.cfgProvider.ChannelConfig()

	// No change:
	// 5. 检查 DA 类型是否需要切换
	// 如果 DA 类型没有变化，直接返回下一个交易数据
	if newCfg.UseBlobs == s.defaultCfg.UseBlobs {
		s.log.Debug("Recomputing optimal ChannelConfig: no need to switch DA type",
			"useBlobs", s.defaultCfg.UseBlobs)
		return s.nextTxData(channel)
	}

	// Change:
	// 6. DA 类型需要切换
	s.log.Info("Recomputing optimal ChannelConfig: changing DA type and requeing blocks...",
		"useBlobsBefore", s.defaultCfg.UseBlobs,
		"useBlobsAfter", newCfg.UseBlobs)
	// 7. 重新排队并更新配置
	s.Requeue(newCfg)
	// 8. 重新获取准备好的通道
	channel, err = s.getReadyChannel(l1Head)
	if err != nil {
		return emptyTxData, err
	}
	// 9. 返回新配置下的下一个交易数据
	return s.nextTxData(channel)
}

// getReadyChannel returns the next channel ready to submit data, or an error.
// getReadyChannel 返回下一个准备提交数据的通道，或者返回错误。
// It will create a new channel if necessary.
// 必要时，它将创建一个新通道。
// If there is no data ready to send, it adds blocks from the block queue
// to the current channel and generates frames for it.
// 如果没有准备发送的数据，它会将块队列中的块添加到当前通道并为其生成帧。
// Always returns nil and the io.EOF sentinel error when
// there is no channel with txData
// 当没有带有 txData 的通道时，始终返回 nil 和 io.EOF 标记错误
func (s *channelManager) getReadyChannel(l1Head eth.BlockID) (*channel, error) {
	// 查找已有的准备好的通道
	var firstWithTxData *channel
	for _, ch := range s.channelQueue {
		if ch.HasTxData() {
			firstWithTxData = ch
			break
		}
	}
	// 检查是否有待处理的数据
	dataPending := firstWithTxData != nil
	s.log.Debug("Requested tx data", "l1Head", l1Head, "txdata_pending", dataPending, "blocks_pending", len(s.blocks))

	// Short circuit if there is pending tx data or the channel manager is closed
	// 如果有待处理的数据,直接返回
	if dataPending {
		return firstWithTxData, nil
	}
	// 检查 channelManager 是否已关闭：
	if s.closed {
		return nil, io.EOF
	}

	// No pending tx data, so we have to add new blocks to the channel

	// If we have no saved blocks, we will not be able to create valid frames
	// 检查是否有待处理的区块：
	if len(s.blocks) == 0 {
		return nil, io.EOF
	}

	// 确保有一个可用的通道
	if err := s.ensureChannelWithSpace(l1Head); err != nil {
		return nil, err
	}

	// 处理待处理的区块，将它们添加到当前通道
	if err := s.processBlocks(); err != nil {
		return nil, err
	}

	// Register current L1 head only after all pending blocks have been
	// processed. Even if a timeout will be triggered now, it is better to have
	// all pending blocks be included in this channel for submission.
	// 注册最新的 L1 区块
	// 仅在所有待处理区块都处理完毕后才注册当前 L1 头。
	// 即使现在会触发超时，最好还是将所有待处理区块都包含在此通道中进行提交。
	s.registerL1Block(l1Head)

	// 生成帧
	if err := s.outputFrames(); err != nil {
		return nil, err
	}

	// 如果当前通道有可用的交易数据，返回它
	if s.currentChannel.HasTxData() {
		return s.currentChannel, nil
	}

	return nil, io.EOF
}

// ensureChannelWithSpace ensures currentChannel is populated with a channel that has
// space for more data (i.e. channel.IsFull returns false). If currentChannel is nil
// or full, a new channel is created.
// ensureChannelWithSpace 确保 currentChannel 中填充了具有更多数据空间的通道（即 channel.IsFull 返回 false）。
// 如果 currentChannel 为 nil 或 已满，则会创建一个新通道。
func (s *channelManager) ensureChannelWithSpace(l1Head eth.BlockID) error {
	if s.currentChannel != nil && !s.currentChannel.IsFull() {
		return nil
	}

	// We reuse the ChannelConfig from the last channel.
	// This will be reassessed at channel submission-time,
	// but this is our best guess at the appropriate values for now.
	cfg := s.defaultCfg
	pc, err := newChannel(s.log, s.metr, cfg, s.rollupCfg, s.l1OriginLastClosedChannel.Number)
	if err != nil {
		return fmt.Errorf("creating new channel: %w", err)
	}

	s.currentChannel = pc
	s.channelQueue = append(s.channelQueue, pc)

	s.log.Info("Created channel",
		"id", pc.ID(),
		"l1Head", l1Head,
		"l1OriginLastClosedChannel", s.l1OriginLastClosedChannel,
		"blocks_pending", len(s.blocks),
		"batch_type", cfg.BatchType,
		"compression_algo", cfg.CompressorConfig.CompressionAlgo,
		"target_num_frames", cfg.TargetNumFrames,
		"max_frame_size", cfg.MaxFrameSize,
		"use_blobs", cfg.UseBlobs,
	)
	s.metr.RecordChannelOpened(pc.ID(), len(s.blocks))

	return nil
}

// registerL1Block registers the given block at the current channel.
// registerL1Block 在当前通道注册给定的块。
func (s *channelManager) registerL1Block(l1Head eth.BlockID) {
	s.currentChannel.CheckTimeout(l1Head.Number)
	s.log.Debug("new L1-block registered at channel builder",
		"l1Head", l1Head,
		"channel_full", s.currentChannel.IsFull(),
		"full_reason", s.currentChannel.FullErr(),
	)
}

// processBlocks adds blocks from the blocks queue to the current channel until
// either the queue got exhausted or the channel is full.
// processBlocks 将块从块队列添加到当前通道，直到 队列耗尽或通道已满
func (s *channelManager) processBlocks() error {
	var (
		blocksAdded int
		_chFullErr  *ChannelFullError // throw away, just for type checking
		latestL2ref eth.L2BlockRef
	)
	// 遍历待处理的L2区块:
	for i, block := range s.blocks {
		// 将 L2 区块添加到当前通道
		l1info, err := s.currentChannel.AddBlock(block)
		// 如果通道已满，停止添加
		if errors.As(err, &_chFullErr) {
			// current block didn't get added because channel is already full
			break
		} else if err != nil {
			return fmt.Errorf("adding block[%d] to channel builder: %w", i, err)
		}
		s.log.Debug("Added block to channel", "id", s.currentChannel.ID(), "block", eth.ToBlockID(block))
		// 增加已添加区块的计数
		blocksAdded += 1
		// 更新最新的 L2 区块引用
		latestL2ref = l2BlockRefFromBlockAndL1Info(block, l1info)
		s.metr.RecordL2BlockInChannel(block)
		// current block got added but channel is now full
		// 检查通道是否已满,如果满了就停止添加
		if s.currentChannel.IsFull() {
			break
		}
	}

	// // 所有区块都处理完,重用切片
	if blocksAdded == len(s.blocks) {
		// all blocks processed, reuse slice
		s.blocks = s.blocks[:0]
	} else {
		// 移除已处理的区块
		// remove processed blocks
		s.blocks = s.blocks[blocksAdded:]
	}

	s.metr.RecordL2BlocksAdded(latestL2ref,
		blocksAdded,
		len(s.blocks),
		s.currentChannel.InputBytes(),
		s.currentChannel.ReadyBytes())
	s.log.Debug("Added blocks to channel",
		"blocks_added", blocksAdded,
		"blocks_pending", len(s.blocks),
		"channel_full", s.currentChannel.IsFull(),
		"input_bytes", s.currentChannel.InputBytes(),
		"ready_bytes", s.currentChannel.ReadyBytes(),
	)
	return nil
}

// outputFrames generates frames for the current channel, and computes and logs the compression ratio
// outputFrames 为当前通道生成帧，并计算和记录压缩比
func (s *channelManager) outputFrames() error {
	// 为当前通道生成帧
	if err := s.currentChannel.OutputFrames(); err != nil {
		return fmt.Errorf("creating frames with channel builder: %w", err)
	}
	if !s.currentChannel.IsFull() {
		return nil
	}

	lastClosedL1Origin := s.currentChannel.LatestL1Origin()
	if lastClosedL1Origin.Number > s.l1OriginLastClosedChannel.Number {
		s.l1OriginLastClosedChannel = lastClosedL1Origin
	}

	inBytes, outBytes := s.currentChannel.InputBytes(), s.currentChannel.OutputBytes()
	s.metr.RecordChannelClosed(
		s.currentChannel.ID(),
		len(s.blocks),
		s.currentChannel.TotalFrames(),
		inBytes,
		outBytes,
		s.currentChannel.FullErr(),
	)

	var comprRatio float64
	if inBytes > 0 {
		comprRatio = float64(outBytes) / float64(inBytes)
	}

	s.log.Info("Channel closed",
		"id", s.currentChannel.ID(),
		"blocks_pending", len(s.blocks),
		"num_frames", s.currentChannel.TotalFrames(),
		"input_bytes", inBytes,
		"output_bytes", outBytes,
		"oldest_l1_origin", s.currentChannel.OldestL1Origin(),
		"l1_origin", lastClosedL1Origin,
		"oldest_l2", s.currentChannel.OldestL2(),
		"latest_l2", s.currentChannel.LatestL2(),
		"full_reason", s.currentChannel.FullErr(),
		"compr_ratio", comprRatio,
		"latest_l1_origin", s.l1OriginLastClosedChannel,
	)
	return nil
}

// AddL2Block adds an L2 block to the internal blocks queue. It returns ErrReorg
// if the block does not extend the last block loaded into the state. If no
// blocks were added yet, the parent hash check is skipped.
// AddL2Block 将 L2 块添加到内部块队列。如果该块未扩展加载到状态中的最后一个块，则返回 ErrReorg
// 如果尚未添加任何块，则跳过父哈希检查。
func (s *channelManager) AddL2Block(block *types.Block) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tip != (common.Hash{}) && s.tip != block.ParentHash() {
		return ErrReorg
	}

	s.metr.RecordL2BlockInPendingQueue(block)
	s.blocks = append(s.blocks, block)
	s.tip = block.Hash()

	return nil
}

// 是一个辅助函数，用于从 L2 区块和 L1 信息生成一个 eth.L2BlockRef 结构体。
// 这个结构体包含了 L2 区块的基本信息以及与 L1 相关的引用信息。
// block *types.Block: L2 区块的指针，包含区块的详细信息。
// l1info *derive.L1BlockInfo: L1 区块信息的指针，包含与 L1 相关的详细信息。
func l2BlockRefFromBlockAndL1Info(block *types.Block, l1info *derive.L1BlockInfo) eth.L2BlockRef {
	return eth.L2BlockRef{
		Hash:           block.Hash(),
		Number:         block.NumberU64(),
		ParentHash:     block.ParentHash(),
		Time:           block.Time(),
		L1Origin:       eth.BlockID{Hash: l1info.BlockHash, Number: l1info.Number},
		SequenceNumber: l1info.SequenceNumber,
	}
}

var ErrPendingAfterClose = errors.New("pending channels remain after closing channel-manager")

// Close clears any pending channels that are not in-flight already, to leave a clean derivation state.
// Close then marks the remaining current open channel, if any, as "full" so it can be submitted as well.
// Close does NOT immediately output frames for the current remaining channel:
// as this might error, due to limitations on a single channel.
// Instead, this is part of the pending-channel submission work: after closing,
// the caller SHOULD drain pending channels by generating TxData repeatedly until there is none left (io.EOF).
// A ErrPendingAfterClose error will be returned if there are any remaining pending channels to submit.
// Close 会清除任何尚未运行的待处理通道，以留下干净的派生状态。
// Close 然后将剩余的当前打开通道（如果有）标记为“已满”，以便也可以提交。
// Close 不会立即输出当前剩余通道的帧：
// 因为这可能会出错，因为单个通道有限制。
// 相反，这是待处理通道提交工作的一部分：关闭后，
// 调用者应该通过反复生成 TxData 来耗尽待处理通道，直到没有剩余通道（io.EOF）。
// 如果有任何剩余的待处理通道要提交，将返回 ErrPendingAfterClose 错误。
func (s *channelManager) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}

	s.closed = true
	s.log.Info("Channel manager is closing")

	// Any pending state can be proactively cleared if there are no submitted transactions
	for _, ch := range s.channelQueue {
		if ch.NoneSubmitted() {
			s.log.Info("Channel has no past or pending submission - dropping", "id", ch.ID())
			s.removePendingChannel(ch)
		} else {
			s.log.Info("Channel is in-flight and will need to be submitted after close", "id", ch.ID(), "confirmed", len(ch.confirmedTransactions), "pending", len(ch.pendingTransactions))
		}
	}
	s.log.Info("Reviewed all pending channels on close", "remaining", len(s.channelQueue))

	if s.currentChannel == nil {
		return nil
	}

	// If the channel is already full, we don't need to close it or output frames.
	// This would already have happened in TxData.
	if !s.currentChannel.IsFull() {
		// Force-close the remaining open channel early (if not already closed):
		// it will be marked as "full" due to service termination.
		s.currentChannel.Close()

		// Final outputFrames call in case there was unflushed data in the compressor.
		if err := s.outputFrames(); err != nil {
			return fmt.Errorf("outputting frames during close: %w", err)
		}
	}

	if s.currentChannel.HasTxData() {
		// Make it clear to the caller that there is remaining pending work.
		return ErrPendingAfterClose
	}
	return nil
}

// Requeue rebuilds the channel manager state by
// rewinding blocks back from the channel queue, and setting the defaultCfg.
// Requeue 通过
// 从通道队列中倒回块并设置 defaultCfg 来重建通道管理器状态。
func (s *channelManager) Requeue(newCfg ChannelConfig) {
	newChannelQueue := []*channel{}
	blocksToRequeue := []*types.Block{}
	for _, channel := range s.channelQueue {
		if !channel.NoneSubmitted() {
			newChannelQueue = append(newChannelQueue, channel)
			continue
		}
		blocksToRequeue = append(blocksToRequeue, channel.channelBuilder.Blocks()...)
	}

	// We put the blocks back at the front of the queue:
	s.blocks = append(blocksToRequeue, s.blocks...)
	// Channels which where already being submitted are put back
	s.channelQueue = newChannelQueue
	s.currentChannel = nil
	// Setting the defaultCfg will cause new channels
	// to pick up the new ChannelConfig
	s.defaultCfg = newCfg
}
