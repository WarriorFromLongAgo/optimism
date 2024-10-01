package batcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	_ "net/http/pprof"
	"sync"
	"time"

	altda "github.com/ethereum-optimism/optimism/op-alt-da"
	"github.com/ethereum-optimism/optimism/op-batcher/metrics"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/dial"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/txmgr"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"golang.org/x/sync/errgroup"
)

var (
	ErrBatcherNotRunning = errors.New("batcher is not running")
	emptyTxData          = txData{
		frames: []frameData{
			{
				data: []byte{},
			},
		},
	}
)

// 用于引用交易，包含交易 ID、是否取消和是否为 Blob 类型的标志。
type txRef struct {
	id       txID
	isCancel bool
	isBlob   bool
}

func (r txRef) String() string {
	return r.string(func(id txID) string { return id.String() })
}

func (r txRef) TerminalString() string {
	return r.string(func(id txID) string { return id.TerminalString() })
}

func (r txRef) string(txIDStringer func(txID) string) string {
	if r.isCancel {
		if r.isBlob {
			return "blob-cancellation"
		} else {
			return "calldata-cancellation"
		}
	}
	return txIDStringer(r.id)
}

// L1Client 定义了与 L1 客户端交互的方法，如获取区块头和账户的 nonce。
type L1Client interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	NonceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (uint64, error)
}

// L2Client 定义了与 L2 客户端交互的方法，如根据区块号获取区块。
type L2Client interface {
	BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error)
}

// RollupClient 定义了与 Rollup 客户端交互的方法，如获取同步状态。
type RollupClient interface {
	SyncStatus(ctx context.Context) (*eth.SyncStatus, error)
}

// DriverSetup is the collection of input/output interfaces and configuration that the driver operates on.
// 包含了 BatchSubmitter 所需的各种配置和客户端实例。
type DriverSetup struct {
	Log              log.Logger
	Metr             metrics.Metricer
	RollupConfig     *rollup.Config
	Config           BatcherConfig
	Txmgr            txmgr.TxManager
	L1Client         L1Client
	EndpointProvider dial.L2EndpointProvider
	ChannelConfig    ChannelConfigProvider
	AltDA            *altda.DAClient
}

// BatchSubmitter encapsulates a service responsible for submitting L2 tx
// batches to L1 for availability.
// 核心结构体，负责管理和提交 L2 交易批次到 L1。
// 包含了各种状态变量、上下文、互斥锁和等待组。
type BatchSubmitter struct {
	DriverSetup

	wg sync.WaitGroup

	shutdownCtx       context.Context
	cancelShutdownCtx context.CancelFunc
	killCtx           context.Context
	cancelKillCtx     context.CancelFunc

	mutex   sync.Mutex
	running bool

	txpoolMutex       sync.Mutex // guards txpoolState and txpoolBlockedBlob
	txpoolState       int
	txpoolBlockedBlob bool

	// lastStoredBlock is the last block loaded into `state`. If it is empty it should be set to the l2 safe head.
	lastStoredBlock eth.BlockID
	lastL1Tip       eth.L1BlockRef

	state *channelManager
}

// NewBatchSubmitter initializes the BatchSubmitter driver from a preconfigured DriverSetup
// NewBatchSubmitter 从预配置的 DriverSetup 初始化 BatchSubmitter 驱动程序
func NewBatchSubmitter(setup DriverSetup) *BatchSubmitter {
	return &BatchSubmitter{
		DriverSetup: setup,
		state:       NewChannelManager(setup.Log, setup.Metr, setup.ChannelConfig, setup.RollupConfig),
	}
}

// StartBatchSubmitting
// 启动批次提交器，设置运行状态和上下文，并启动主循环。
// 检查是否已经在运行，初始化上下文，清除状态，并启动主循环。
func (l *BatchSubmitter) StartBatchSubmitting() error {
	l.Log.Info("Starting Batch Submitter")

	l.mutex.Lock()
	defer l.mutex.Unlock()

	if l.running {
		return errors.New("batcher is already running")
	}
	l.running = true

	// 创建两个上下文：shutdownCtx 和 killCtx，并分别设置取消函数。
	l.shutdownCtx, l.cancelShutdownCtx = context.WithCancel(context.Background())
	l.killCtx, l.cancelKillCtx = context.WithCancel(context.Background())
	// 调用 clearState 方法清除批次提交器的状态。
	l.clearState(l.shutdownCtx)
	// 重置最后存储的区块:
	l.lastStoredBlock = eth.BlockID{}

	// 如果配置中设置了 WaitNodeSync，
	if l.Config.WaitNodeSync {
		// 则调用 waitNodeSync 方法等待节点同步完成。
		err := l.waitNodeSync()
		if err != nil {
			return fmt.Errorf("error waiting for node sync: %w", err)
		}
	}

	l.wg.Add(1)
	// 启动主循环: 并启动一个新的 goroutine 运行 loop 方法。
	go l.loop()

	l.Log.Info("Batch Submitter started")
	return nil
}

// StopBatchSubmittingIfRunning 停止批次提交器，处理上下文取消和等待组。
func (l *BatchSubmitter) StopBatchSubmittingIfRunning(ctx context.Context) error {
	err := l.StopBatchSubmitting(ctx)
	if errors.Is(err, ErrBatcherNotRunning) {
		return nil
	}
	return err
}

// StopBatchSubmitting stops the batch-submitter loop, and force-kills if the provided ctx is done.
// 检查是否在运行，取消上下文，并等待所有 goroutine 结束。
func (l *BatchSubmitter) StopBatchSubmitting(ctx context.Context) error {
	l.Log.Info("Stopping Batch Submitter")

	l.mutex.Lock()
	defer l.mutex.Unlock()

	if !l.running {
		return ErrBatcherNotRunning
	}
	l.running = false

	// go routine will call cancelKill() if the passed in ctx is ever Done
	cancelKill := l.cancelKillCtx
	wrapped, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-wrapped.Done()
		cancelKill()
	}()

	l.cancelShutdownCtx()
	l.wg.Wait()
	l.cancelKillCtx()

	l.Log.Info("Batch Submitter stopped")
	return nil
}

// loadBlocksIntoState loads all blocks since the previous stored block
// It does the following:
// 1. Fetch the sync status of the sequencer
// 2. Check if the sync status is valid or if we are all the way up to date
// 3. Check if it needs to initialize state OR it is lagging (todo: lagging just means race condition?)
// 4. Load all new blocks into the local state.
//
// If there is a reorg, it will reset the last stored block but not clear the internal state so
// the state can be flushed to L1.

// loadBlocksIntoState 加载自上一个存储块以来的所有块
// 它执行以下操作：
// 1. 获取序列器的同步状态
// 2. 检查同步状态是否有效或我们是否一直保持最新状态
// 3. 检查是否需要初始化状态或是否滞后（todo：滞后只是意味着竞争条件？）
// 4. 将所有新块加载到本地状态。
// 如果有重组，它将重置最后存储的块但不清除内部状态，以便状态可以刷新到 L1。
func (l *BatchSubmitter) loadBlocksIntoState(ctx context.Context) error {
	start, end, err := l.calculateL2BlockRangeToStore(ctx)
	if err != nil {
		l.Log.Warn("Error calculating L2 block range", "err", err)
		return err
	} else if start.Number >= end.Number {
		return errors.New("start number is >= end number")
	}

	var latestBlock *types.Block
	// Add all blocks to "state"
	for i := start.Number + 1; i < end.Number+1; i++ {
		block, err := l.loadBlockIntoState(ctx, i)
		if errors.Is(err, ErrReorg) {
			l.Log.Warn("Found L2 reorg", "block_number", i)
			l.lastStoredBlock = eth.BlockID{}
			return err
		} else if err != nil {
			l.Log.Warn("Failed to load block into state", "err", err)
			return err
		}
		l.lastStoredBlock = eth.ToBlockID(block)
		latestBlock = block
	}

	l2ref, err := derive.L2BlockToBlockRef(l.RollupConfig, latestBlock)
	if err != nil {
		l.Log.Warn("Invalid L2 block loaded into state", "err", err)
		return err
	}

	l.Metr.RecordL2BlocksLoaded(l2ref)
	return nil
}

// loadBlockIntoState fetches & stores a single block into `state`. It returns the block it loaded.
// 获取单个块并将其存储到“状态”中。它返回其加载的块。
func (l *BatchSubmitter) loadBlockIntoState(ctx context.Context, blockNumber uint64) (*types.Block, error) {
	l2Client, err := l.EndpointProvider.EthClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting L2 client: %w", err)
	}

	cCtx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()

	block, err := l2Client.BlockByNumber(cCtx, new(big.Int).SetUint64(blockNumber))
	if err != nil {
		return nil, fmt.Errorf("getting L2 block: %w", err)
	}

	if err := l.state.AddL2Block(block); err != nil {
		return nil, fmt.Errorf("adding L2 block to state: %w", err)
	}

	l.Log.Info("Added L2 block to local state", "block", eth.ToBlockID(block), "tx_count", len(block.Transactions()), "time", block.Time())
	return block, nil
}

// calculateL2BlockRangeToStore determines the range (start,end] that should be loaded into the local state.
// It also takes care of initializing some local state (i.e. will modify l.lastStoredBlock in certain conditions)
// calculateL2BlockRangeToStore 确定应加载到本地状态的范围（开始，结束]。
// 它还负责初始化一些本地状态（即在某些条件下修改 l.lastStoredBlock）
func (l *BatchSubmitter) calculateL2BlockRangeToStore(ctx context.Context) (eth.BlockID, eth.BlockID, error) {
	rollupClient, err := l.EndpointProvider.RollupClient(ctx)
	if err != nil {
		return eth.BlockID{}, eth.BlockID{}, fmt.Errorf("getting rollup client: %w", err)
	}

	cCtx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()

	syncStatus, err := rollupClient.SyncStatus(cCtx)
	// Ensure that we have the sync status
	if err != nil {
		return eth.BlockID{}, eth.BlockID{}, fmt.Errorf("failed to get sync status: %w", err)
	}
	if syncStatus.HeadL1 == (eth.L1BlockRef{}) {
		return eth.BlockID{}, eth.BlockID{}, errors.New("empty sync status")
	}

	// Check last stored to see if it needs to be set on startup OR set if is lagged behind.
	// It lagging implies that the op-node processed some batches that were submitted prior to the current instance of the batcher being alive.
	if l.lastStoredBlock == (eth.BlockID{}) {
		l.Log.Info("Starting batch-submitter work at safe-head", "safe", syncStatus.SafeL2)
		l.lastStoredBlock = syncStatus.SafeL2.ID()
	} else if l.lastStoredBlock.Number < syncStatus.SafeL2.Number {
		l.Log.Warn("Last submitted block lagged behind L2 safe head: batch submission will continue from the safe head now", "last", l.lastStoredBlock, "safe", syncStatus.SafeL2)
		l.lastStoredBlock = syncStatus.SafeL2.ID()
	}

	// Check if we should even attempt to load any blocks. TODO: May not need this check
	if syncStatus.SafeL2.Number >= syncStatus.UnsafeL2.Number {
		return eth.BlockID{}, eth.BlockID{}, fmt.Errorf("L2 safe head(%d) ahead of L2 unsafe head(%d)", syncStatus.SafeL2.Number, syncStatus.UnsafeL2.Number)
	}

	return l.lastStoredBlock, syncStatus.UnsafeL2.ID(), nil
}

// The following things occur:
// New L2 block (reorg or not)
// L1 transaction is confirmed
//
// What the batcher does:
// Ensure that channels are created & submitted as frames for an L2 range
//
// Error conditions:
// Submitted batch, but it is not valid
// Missed L2 block somehow.

// 发生以下情况：
// 新的 L2 块（重组或不重组）
// L1 交易已确认
// 批处理程序执行的操作：
// 确保已创建通道并将其作为 L2 范围的帧提交
// 错误条件：
// 已提交批处理，但无效
// 不知何故错过了 L2 块。

const (
	// TxpoolGood Txpool states.  Possible state transitions:
	//   TxpoolGood -> TxpoolBlocked:
	//     happens when ErrAlreadyReserved is ever returned by the TxMgr.
	//   TxpoolBlocked -> TxpoolCancelPending:
	//     happens once the send loop detects the txpool is blocked, and results in attempting to
	//     send a cancellation transaction.
	//   TxpoolCancelPending -> TxpoolGood:
	//     happens once the cancel transaction completes, whether successfully or in error.

	// TxpoolGood Txpool 状态。可能的状态转换：
	// TxpoolGood -> TxpoolBlocked：
	// 当 TxMgr 返回 ErrAlreadyReserved 时发生。
	// TxpoolBlocked -> TxpoolCancelPending：
	// 一旦发送循环检测到 txpool 被阻止，并导致尝试
	// 发送取消交易，就会发生。
	// TxpoolCancelPending -> TxpoolGood：
	// 一旦取消交易完成，无论成功还是错误，都会发生。
	TxpoolGood int = iota
	TxpoolBlocked
	TxpoolCancelPending
)

// 主循环，定期检查和处理交易池状态，加载区块并发布状态到 L1。
// 启动接收处理循环，定期检查交易池状态，加载区块并发布状态。
func (l *BatchSubmitter) loop() {
	// 负责定期检查和处理交易池状态，加载区块并发布状态到 L1。
	defer l.wg.Done()

	// 创建接收通道 receiptsCh
	receiptsCh := make(chan txmgr.TxReceipt[txRef])
	// 交易队列 queue。
	queue := txmgr.NewQueue[txRef](l.killCtx, l.Txmgr, l.Config.MaxPendingTransactions)
	// 创建错误组 daGroup，并根据配置设置并发限制。
	daGroup := &errgroup.Group{}
	// errgroup with limit of 0 means no goroutine is able to run concurrently,
	// so we only set the limit if it is greater than 0.
	// errgroup 的限制为 0 意味着没有 goroutine 能够并发运行，
	// 因此我们仅当限制大于 0 时才设置限制。
	if l.Config.MaxConcurrentDARequests > 0 {
		daGroup.SetLimit(int(l.Config.MaxConcurrentDARequests))
	}

	// start the receipt/result processing loop
	receiptLoopDone := make(chan struct{})
	defer close(receiptLoopDone) // shut down receipt loop

	l.txpoolMutex.Lock()
	l.txpoolState = TxpoolGood
	l.txpoolMutex.Unlock()
	go func() {
		for {
			select {
			case r := <-receiptsCh:
				l.txpoolMutex.Lock()
				if errors.Is(r.Err, txpool.ErrAlreadyReserved) && l.txpoolState == TxpoolGood {
					l.txpoolState = TxpoolBlocked
					l.txpoolBlockedBlob = r.ID.isBlob
					l.Log.Info("incompatible tx in txpool", "is_blob", r.ID.isBlob)
				} else if r.ID.isCancel && l.txpoolState == TxpoolCancelPending {
					// Set state to TxpoolGood even if the cancellation transaction ended in error
					// since the stuck transaction could have cleared while we were waiting.
					l.txpoolState = TxpoolGood
					l.Log.Info("txpool may no longer be blocked", "err", r.Err)
				}
				l.txpoolMutex.Unlock()
				l.Log.Info("Handling receipt", "id", r.ID)
				l.handleReceipt(r)
			case <-receiptLoopDone:
				l.Log.Info("Receipt processing loop done")
				return
			}
		}
	}()

	ticker := time.NewTicker(l.Config.PollInterval)
	defer ticker.Stop()

	publishAndWait := func() {
		l.publishStateToL1(queue, receiptsCh, daGroup)
		if !l.Txmgr.IsClosed() {
			if l.Config.UseAltDA {
				l.Log.Info("Waiting for altDA writes to complete...")
				err := daGroup.Wait()
				if err != nil {
					l.Log.Error("Error returned by one of the altda goroutines waited on", "err", err)
				}
			}
			l.Log.Info("Waiting for L1 txs to be confirmed...")
			err := queue.Wait()
			if err != nil {
				l.Log.Error("Error returned by one of the txmgr goroutines waited on", "err", err)
			}
		} else {
			l.Log.Info("Txmgr is closed, remaining channel data won't be sent")
		}
	}

	for {
		select {
		case <-ticker.C:
			if !l.checkTxpool(queue, receiptsCh) {
				continue
			}
			if err := l.loadBlocksIntoState(l.shutdownCtx); errors.Is(err, ErrReorg) {
				err := l.state.Close()
				if err != nil {
					if errors.Is(err, ErrPendingAfterClose) {
						l.Log.Warn("Closed channel manager to handle L2 reorg with pending channel(s) remaining - submitting")
					} else {
						l.Log.Error("Error closing the channel manager to handle a L2 reorg", "err", err)
					}
				}
				// on reorg we want to publish all pending state then wait until each result clears before resetting
				// the state.
				publishAndWait()
				l.clearState(l.shutdownCtx)
				continue
			}
			l.publishStateToL1(queue, receiptsCh, daGroup)
		case <-l.shutdownCtx.Done():
			if l.Txmgr.IsClosed() {
				l.Log.Info("Txmgr is closed, remaining channel data won't be sent")
				return
			}
			// This removes any never-submitted pending channels, so these do not have to be drained with transactions.
			// Any remaining unfinished channel is terminated, so its data gets submitted.
			err := l.state.Close()
			if err != nil {
				if errors.Is(err, ErrPendingAfterClose) {
					l.Log.Warn("Closed channel manager on shutdown with pending channel(s) remaining - submitting")
				} else {
					l.Log.Error("Error closing the channel manager on shutdown", "err", err)
				}
			}
			publishAndWait()
			l.Log.Info("Finished publishing all remaining channel data")
			return
		}
	}
}

// waitNodeSync Check to see if there was a batcher tx sent recently that
// still needs more block confirmations before being considered finalized
// waitNodeSync 检查最近是否发送了批处理 tx，在被视为最终确定之前仍需要更多块确认
// waitNodeSync 方法的主要功能是检查最近是否发送了批处理交易，并等待节点同步到最新的 L1 区块，以确保交易被视为最终确定。
func (l *BatchSubmitter) waitNodeSync() error {
	ctx := l.shutdownCtx
	// 使用 EndpointProvider 获取 Rollup 客户端。
	rollupClient, err := l.EndpointProvider.RollupClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to get rollup client: %w", err)
	}

	// 创建一个带有超时的上下文 cCtx，超时时间为 NetworkTimeout。
	cCtx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()

	// 调用 l1Tip 方法获取当前的 L1 最新区块。
	l1Tip, err := l.l1Tip(cCtx)
	if err != nil {
		return fmt.Errorf("failed to retrieve l1 tip: %w", err)
	}

	// 将 l1Tip.Number 设为目标区块 l1TargetBlock。
	l1TargetBlock := l1Tip.Number
	// 如果配置中设置了 CheckRecentTxsDepth，则检查最近的批处理交易。
	if l.Config.CheckRecentTxsDepth != 0 {
		l.Log.Info("Checking for recently submitted batcher transactions on L1")
		recentBlock, found, err := eth.CheckRecentTxs(cCtx, l.L1Client, l.Config.CheckRecentTxsDepth, l.Txmgr.From())
		if err != nil {
			return fmt.Errorf("failed checking recent batcher txs: %w", err)
		}
		l.Log.Info("Checked for recently submitted batcher transactions on L1",
			"l1_head", l1Tip, "l1_recent", recentBlock, "found", found)
		l1TargetBlock = recentBlock
	}
	// 调用 dial.WaitRollupSync 方法，等待 Rollup 客户端同步到目标区块 l1TargetBlock，确保交易被视为最终确定。
	return dial.WaitRollupSync(l.shutdownCtx, l.Log, rollupClient, l1TargetBlock, time.Second*12)
}

// publishStateToL1 queues up all pending TxData to be published to the L1, returning when there is
// no more data to queue for publishing or if there was an error queing the data.
// publishStateToL1 将所有待处理的 TxData 排队以发布到 L1，当没有更多数据排队发布或排队数据时出现错误时返回。
func (l *BatchSubmitter) publishStateToL1(queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef], daGroup *errgroup.Group) {
	for {
		// if the txmgr is closed, we stop the transaction sending
		if l.Txmgr.IsClosed() {
			l.Log.Info("Txmgr is closed, aborting state publishing")
			return
		}
		if !l.checkTxpool(queue, receiptsCh) {
			l.Log.Info("txpool state is not good, aborting state publishing")
			return
		}
		err := l.publishTxToL1(l.killCtx, queue, receiptsCh, daGroup)
		if err != nil {
			if err != io.EOF {
				l.Log.Error("Error publishing tx to l1", "err", err)
			}
			return
		}
	}
}

// clearState clears the state of the channel manager
// clearState 清除频道管理器的状态
func (l *BatchSubmitter) clearState(ctx context.Context) {
	l.Log.Info("Clearing state")
	defer l.Log.Info("State cleared")

	clearStateWithL1Origin := func() bool {
		// 该函数尝试获取 L1 安全起源，并使用该起源清除状态。
		l1SafeOrigin, err := l.safeL1Origin(ctx)
		if err != nil {
			l.Log.Warn("Failed to query L1 safe origin, will retry", "err", err)
			return false
		} else {
			l.Log.Info("Clearing state with safe L1 origin", "origin", l1SafeOrigin)
			l.state.Clear(l1SafeOrigin)
			return true
		}
	}

	// Attempt to set the L1 safe origin and clear the state, if fetching fails -- fall through to an infinite retry
	if clearStateWithL1Origin() {
		return
	}

	// 如果初次尝试失败，设置一个定时器，每 5 秒重试一次。
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	// 进入一个无限循环，直到成功清除状态或上下文被取消。
	for {
		select {
		case <-tick.C:
			if clearStateWithL1Origin() {
				return
			}
		case <-ctx.Done():
			l.Log.Warn("Clearing state cancelled")
			l.state.Clear(eth.BlockID{})
			return
		}
	}
}

// publishTxToL1 submits a single state tx to the L1
// publishTxToL1 向 L1 提交单个状态交易
func (l *BatchSubmitter) publishTxToL1(ctx context.Context, queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef], daGroup *errgroup.Group) error {
	// send all available transactions
	l1tip, err := l.l1Tip(ctx)
	if err != nil {
		l.Log.Error("Failed to query L1 tip", "err", err)
		return err
	}
	l.recordL1Tip(l1tip)

	// Collect next transaction data. This pulls data out of the channel, so we need to make sure
	// to put it back if ever da or txmgr requests fail, by calling l.recordFailedDARequest/recordFailedTx.
	txdata, err := l.state.TxData(l1tip.ID())

	if err == io.EOF {
		l.Log.Trace("No transaction data available")
		return err
	} else if err != nil {
		l.Log.Error("Unable to get tx data", "err", err)
		return err
	}

	if err = l.sendTransaction(txdata, queue, receiptsCh, daGroup); err != nil {
		return fmt.Errorf("BatchSubmitter.sendTransaction failed: %w", err)
	}
	return nil
}

func (l *BatchSubmitter) safeL1Origin(ctx context.Context) (eth.BlockID, error) {
	c, err := l.EndpointProvider.RollupClient(ctx)
	if err != nil {
		log.Error("Failed to get rollup client", "err", err)
		return eth.BlockID{}, fmt.Errorf("safe l1 origin: error getting rollup client: %w", err)
	}

	cCtx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()

	status, err := c.SyncStatus(cCtx)
	if err != nil {
		log.Error("Failed to get sync status", "err", err)
		return eth.BlockID{}, fmt.Errorf("safe l1 origin: error getting sync status: %w", err)
	}

	// If the safe L2 block origin is 0, we are at the genesis block and should use the L1 origin from the rollup config.
	if status.SafeL2.L1Origin.Number == 0 {
		return l.RollupConfig.Genesis.L1, nil
	}

	return status.SafeL2.L1Origin, nil
}

// cancelBlockingTx creates an empty transaction of appropriate type to cancel out the incompatible
// transaction stuck in the txpool. In the future we might send an actual batch transaction instead
// of an empty one to avoid wasting the tx fee.
// cancelBlockingTx 创建适当类型的空交易以取消交易池中卡住的不兼容交易。将来我们可能会发送实际的批量交易而不是空交易，以避免浪费交易费。
func (l *BatchSubmitter) cancelBlockingTx(queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef], isBlockedBlob bool) {
	var candidate *txmgr.TxCandidate
	var err error
	if isBlockedBlob {
		candidate = l.calldataTxCandidate([]byte{})
	} else if candidate, err = l.blobTxCandidate(emptyTxData); err != nil {
		panic(err) // this error should not happen
	}
	l.Log.Warn("sending a cancellation transaction to unblock txpool", "blocked_blob", isBlockedBlob)
	l.sendTx(txData{}, true, candidate, queue, receiptsCh)
}

// publishToAltDAAndL1 posts the txdata to the DA Provider and then sends the commitment to L1.
// publishToAltDAAndL1 将 txdata 发布到 DA 提供商，然后将承诺发送给 L1。
func (l *BatchSubmitter) publishToAltDAAndL1(txdata txData, queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef], daGroup *errgroup.Group) {
	// sanity checks
	if nf := len(txdata.frames); nf != 1 {
		l.Log.Crit("Unexpected number of frames in calldata tx", "num_frames", nf)
	}
	if txdata.asBlob {
		l.Log.Crit("Unexpected blob txdata with AltDA enabled")
	}

	// when posting txdata to an external DA Provider, we use a goroutine to avoid blocking the main loop
	// since it may take a while for the request to return.
	goroutineSpawned := daGroup.TryGo(func() error {
		// TODO: probably shouldn't be using the global shutdownCtx here, see https://go.dev/blog/context-and-structs
		// but sendTransaction receives l.killCtx as an argument, which currently is only canceled after waiting for the main loop
		// to exit, which would wait on this DA call to finish, which would take a long time.
		// So we prefer to mimic the behavior of txmgr and cancel all pending DA/txmgr requests when the batcher is stopped.
		comm, err := l.AltDA.SetInput(l.shutdownCtx, txdata.CallData())
		if err != nil {
			l.Log.Error("Failed to post input to Alt DA", "error", err)
			// requeue frame if we fail to post to the DA Provider so it can be retried
			// note: this assumes that the da server caches requests, otherwise it might lead to resubmissions of the blobs
			l.recordFailedDARequest(txdata.ID(), err)
			return nil
		}
		l.Log.Info("Set altda input", "commitment", comm, "tx", txdata.ID())
		candidate := l.calldataTxCandidate(comm.TxData())
		l.sendTx(txdata, false, candidate, queue, receiptsCh)
		return nil
	})
	if !goroutineSpawned {
		// We couldn't start the goroutine because the errgroup.Group limit
		// is already reached. Since we can't send the txdata, we have to
		// return it for later processing. We use nil error to skip error logging.
		l.recordFailedDARequest(txdata.ID(), nil)
	}
}

// sendTransaction creates & queues for sending a transaction to the batch inbox address with the given `txData`.
// This call will block if the txmgr queue is at the  max-pending limit.
// The method will block if the queue's MaxPendingTransactions is exceeded.
// sendTransaction 创建并排队，用于使用给定的“txData”将交易发送到批处理收件箱地址。
// 如果 txmgr 队列达到最大待处理限制，则此调用将阻塞。
// 如果超出队列的 MaxPendingTransactions，则该方法将阻塞。
func (l *BatchSubmitter) sendTransaction(txdata txData, queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef], daGroup *errgroup.Group) error {
	var err error

	// if Alt DA is enabled we post the txdata to the DA Provider and replace it with the commitment.
	if l.Config.UseAltDA {
		l.publishToAltDAAndL1(txdata, queue, receiptsCh, daGroup)
		// we return nil to allow publishStateToL1 to keep processing the next txdata
		return nil
	}

	var candidate *txmgr.TxCandidate
	if txdata.asBlob {
		if candidate, err = l.blobTxCandidate(txdata); err != nil {
			// We could potentially fall through and try a calldata tx instead, but this would
			// likely result in the chain spending more in gas fees than it is tuned for, so best
			// to just fail. We do not expect this error to trigger unless there is a serious bug
			// or configuration issue.
			return fmt.Errorf("could not create blob tx candidate: %w", err)
		}
	} else {
		// sanity check
		if nf := len(txdata.frames); nf != 1 {
			l.Log.Crit("Unexpected number of frames in calldata tx", "num_frames", nf)
		}
		candidate = l.calldataTxCandidate(txdata.CallData())
	}

	l.sendTx(txdata, false, candidate, queue, receiptsCh)
	return nil
}

// sendTx uses the txmgr queue to send the given transaction candidate after setting its
// gaslimit. It will block if the txmgr queue has reached its MaxPendingTransactions limit.
// sendTx 在设置其 gaslimit 后使用 txmgr 队列发送给定的交易候选。如果 txmgr 队列已达到其 MaxPendingTransactions 限制，它将被阻止。
func (l *BatchSubmitter) sendTx(txdata txData, isCancel bool, candidate *txmgr.TxCandidate, queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef]) {
	intrinsicGas, err := core.IntrinsicGas(candidate.TxData, nil, false, true, true, false)
	if err != nil {
		// we log instead of return an error here because txmgr can do its own gas estimation
		l.Log.Error("Failed to calculate intrinsic gas", "err", err)
	} else {
		candidate.GasLimit = intrinsicGas
	}

	queue.Send(txRef{id: txdata.ID(), isCancel: isCancel, isBlob: txdata.asBlob}, *candidate, receiptsCh)
}

func (l *BatchSubmitter) blobTxCandidate(data txData) (*txmgr.TxCandidate, error) {
	blobs, err := data.Blobs()
	if err != nil {
		return nil, fmt.Errorf("generating blobs for tx data: %w", err)
	}
	size := data.Len()
	lastSize := len(data.frames[len(data.frames)-1].data)
	l.Log.Info("Building Blob transaction candidate",
		"size", size, "last_size", lastSize, "num_blobs", len(blobs))
	l.Metr.RecordBlobUsedBytes(lastSize)
	return &txmgr.TxCandidate{
		To:    &l.RollupConfig.BatchInboxAddress,
		Blobs: blobs,
	}, nil
}

func (l *BatchSubmitter) calldataTxCandidate(data []byte) *txmgr.TxCandidate {
	l.Log.Info("Building Calldata transaction candidate", "size", len(data))
	return &txmgr.TxCandidate{
		To:     &l.RollupConfig.BatchInboxAddress,
		TxData: data,
	}
}

func (l *BatchSubmitter) handleReceipt(r txmgr.TxReceipt[txRef]) {
	// Record TX Status
	if r.Err != nil {
		l.recordFailedTx(r.ID.id, r.Err)
	} else {
		l.recordConfirmedTx(r.ID.id, r.Receipt)
	}
}

func (l *BatchSubmitter) recordL1Tip(l1tip eth.L1BlockRef) {
	if l.lastL1Tip == l1tip {
		return
	}
	l.lastL1Tip = l1tip
	l.Metr.RecordLatestL1Block(l1tip)
}

func (l *BatchSubmitter) recordFailedDARequest(id txID, err error) {
	if err != nil {
		l.Log.Warn("DA request failed", logFields(id, err)...)
	}
	l.state.TxFailed(id)
}

func (l *BatchSubmitter) recordFailedTx(id txID, err error) {
	l.Log.Warn("Transaction failed to send", logFields(id, err)...)
	l.state.TxFailed(id)
}

func (l *BatchSubmitter) recordConfirmedTx(id txID, receipt *types.Receipt) {
	l.Log.Info("Transaction confirmed", logFields(id, receipt)...)
	l1block := eth.ReceiptBlockID(receipt)
	l.state.TxConfirmed(id, l1block)
}

// l1Tip gets the current L1 tip as a L1BlockRef. The passed context is assumed
// to be a lifetime context, so it is internally wrapped with a network timeout.
// l1Tip 将当前 L1 提示作为 L1BlockRef 获取。传递的上下文被假定为
// 终身上下文，因此它在内部包装了网络超时。
func (l *BatchSubmitter) l1Tip(ctx context.Context) (eth.L1BlockRef, error) {
	// 方法用于获取当前 L1 区块链的最新区块（L1 Tip），并将其转换为 eth.L1BlockRef 结构体

	tctx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()
	head, err := l.L1Client.HeaderByNumber(tctx, nil)
	if err != nil {
		return eth.L1BlockRef{}, fmt.Errorf("getting latest L1 block: %w", err)
	}
	return eth.InfoToL1BlockRef(eth.HeaderBlockInfo(head)), nil
}

func (l *BatchSubmitter) checkTxpool(queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef]) bool {
	l.txpoolMutex.Lock()
	if l.txpoolState == TxpoolBlocked {
		// txpoolState is set to Blocked only if Send() is returning
		// ErrAlreadyReserved. In this case, the TxMgr nonce should be reset to nil,
		// allowing us to send a cancellation transaction.
		l.txpoolState = TxpoolCancelPending
		isBlob := l.txpoolBlockedBlob
		l.txpoolMutex.Unlock()
		l.cancelBlockingTx(queue, receiptsCh, isBlob)
		return false
	}
	r := l.txpoolState == TxpoolGood
	l.txpoolMutex.Unlock()
	return r
}

func logFields(xs ...any) (fs []any) {
	for _, x := range xs {
		switch v := x.(type) {
		case txID:
			fs = append(fs, "tx_id", v.String())
		case *types.Receipt:
			fs = append(fs, "tx", v.TxHash, "block", eth.ReceiptBlockID(v))
		case error:
			fs = append(fs, "err", v)
		default:
			fs = append(fs, "ERROR", fmt.Sprintf("logFields: unknown type: %T", x))
		}
	}
	return fs
}
