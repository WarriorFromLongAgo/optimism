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

// loadBlocksIntoState 主要功能是将新的 L2 区块加载到批处理提交器的本地状态中
// 它执行以下操作：
// 1. 获取序列器的同步状态
// 2. 检查同步状态是否有效或我们是否一直保持最新状态
// 3. 检查是否需要初始化状态或是否滞后（todo：滞后只是意味着竞争条件？）
// 4. 将所有新块加载到本地状态。
// 如果有重组，它将重置最后存储的块但不清除内部状态，以便状态可以刷新到 L1。
func (l *BatchSubmitter) loadBlocksIntoState(ctx context.Context) error {
	// 1. 计算需要加载的 L2 区块范围
	start, end, err := l.calculateL2BlockRangeToStore(ctx)
	if err != nil {
		l.Log.Warn("Error calculating L2 block range", "err", err)
		return err
	} else if start.Number >= end.Number {
		return errors.New("start number is >= end number")
	}

	var latestBlock *types.Block
	// 2. 遍历计算出的区块范围,逐个加载区块
	// Add all blocks to "state"
	for i := start.Number + 1; i < end.Number+1; i++ {
		// 3. 加载单个区块到状态
		block, err := l.loadBlockIntoState(ctx, i)
		if errors.Is(err, ErrReorg) {
			// 4. 如果检测到重组,重置最后存储的区块并返回错误
			l.Log.Warn("Found L2 reorg", "block_number", i)
			l.lastStoredBlock = eth.BlockID{}
			return err
		} else if err != nil {
			// 5. 如果加载失败,记录错误并返回
			l.Log.Warn("Failed to load block into state", "err", err)
			return err
		}
		// 6. 更新最后存储的区块信息
		l.lastStoredBlock = eth.ToBlockID(block)
		latestBlock = block
	}

	// 7. 将最新加载的区块转换为 L2BlockRef
	l2ref, err := derive.L2BlockToBlockRef(l.RollupConfig, latestBlock)
	if err != nil {
		l.Log.Warn("Invalid L2 block loaded into state", "err", err)
		return err
	}

	// 8. 记录加载的 L2 区块信息
	l.Metr.RecordL2BlocksLoaded(l2ref)
	return nil
}

// loadBlockIntoState fetches & stores a single block into `state`. It returns the block it loaded.
// 获取单个块并将其存储到“状态”中。它返回其加载的块。
// 主要功能是从 L2 网络获取指定区块号的区块,并将其加载到批处理提交器的本地状态中。
func (l *BatchSubmitter) loadBlockIntoState(ctx context.Context, blockNumber uint64) (*types.Block, error) {
	l2Client, err := l.EndpointProvider.EthClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting L2 client: %w", err)
	}

	cCtx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()

	// 3. 从 L2 网络获取指定区块号的区块
	block, err := l2Client.BlockByNumber(cCtx, new(big.Int).SetUint64(blockNumber))
	if err != nil {
		return nil, fmt.Errorf("getting L2 block: %w", err)
	}

	// 4. 将获取的区块添加到本地状态
	if err := l.state.AddL2Block(block); err != nil {
		return nil, fmt.Errorf("adding L2 block to state: %w", err)
	}

	l.Log.Info("Added L2 block to local state", "block", eth.ToBlockID(block), "tx_count", len(block.Transactions()), "time", block.Time())
	return block, nil
}

// calculateL2BlockRangeToStore determines the range (start,end] that should be loaded into the local state.
// It also takes care of initializing some local state (i.e. will modify l.lastStoredBlock in certain conditions)
// calculateL2BlockRangeToStore 确定应加载到本地状态的 L2 区块范围（开始，结束]。
// 它还负责初始化一些本地状态（即在某些条件下修改 l.lastStoredBlock）
// 主要功能是确定需要加载到本地状态的 L2 区块范围。
func (l *BatchSubmitter) calculateL2BlockRangeToStore(ctx context.Context) (eth.BlockID, eth.BlockID, error) {
	// 1. 获取 Rollup 客户端
	rollupClient, err := l.EndpointProvider.RollupClient(ctx)
	if err != nil {
		return eth.BlockID{}, eth.BlockID{}, fmt.Errorf("getting rollup client: %w", err)
	}

	// 2. 创建一个带超时的上下文
	cCtx, cancel := context.WithTimeout(ctx, l.Config.NetworkTimeout)
	defer cancel()

	// 3. 获取同步状态
	syncStatus, err := rollupClient.SyncStatus(cCtx)
	// Ensure that we have the sync status
	// 4. 确保我们成功获取了同步状态
	if err != nil {
		return eth.BlockID{}, eth.BlockID{}, fmt.Errorf("failed to get sync status: %w", err)
	}
	if syncStatus.HeadL1 == (eth.L1BlockRef{}) {
		return eth.BlockID{}, eth.BlockID{}, errors.New("empty sync status")
	}

	// Check last stored to see if it needs to be set on startup OR set if is lagged behind.
	// It lagging implies that the op-node processed some batches that were submitted prior to the current instance of the batcher being alive.
	// 5. 检查并更新 lastStoredBlock
	// 这个步骤处理启动时的初始化或者当 batcher 落后时的情况

	// 5.1 如果 lastStoredBlock 为空,从安全头开始
	if l.lastStoredBlock == (eth.BlockID{}) {
		l.Log.Info("Starting batch-submitter work at safe-head", "safe", syncStatus.SafeL2)
		l.lastStoredBlock = syncStatus.SafeL2.ID()
	} else if l.lastStoredBlock.Number < syncStatus.SafeL2.Number {
		// 5.2 如果 lastStoredBlock 落后于安全头,更新到安全头
		l.Log.Warn("Last submitted block lagged behind L2 safe head: batch submission will continue from the safe head now", "last", l.lastStoredBlock, "safe", syncStatus.SafeL2)
		l.lastStoredBlock = syncStatus.SafeL2.ID()
	}

	// 6. 检查是否应该尝试加载任何区块
	// 如果安全头大于或等于不安全头,则没有新的区块需要加载
	// Check if we should even attempt to load any blocks. TODO: May not need this check
	if syncStatus.SafeL2.Number >= syncStatus.UnsafeL2.Number {
		return eth.BlockID{}, eth.BlockID{}, fmt.Errorf("L2 safe head(%d) ahead of L2 unsafe head(%d)", syncStatus.SafeL2.Number, syncStatus.UnsafeL2.Number)
	}

	// 7. 返回计算出的区块范围
	// 开始区块是 lastStoredBlock,结束区块是不安全头
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

	// TxpoolGood 表示交易可以正常提交，没有阻塞或其他问题。这是交易池的默认和理想状态。
	TxpoolGood int = iota
	// TxpoolBlocked 当发现有不兼容的交易阻塞了交易池时，状态会被设置为 TxpoolBlocked。
	// 这通常发生在 Send() 方法返回 ErrAlreadyReserved 错误时。这个状态表明需要采取行动来解除阻塞，比如发送一个取消交易。
	TxpoolBlocked
	// TxpoolCancelPending 当系统检测到交易池被阻塞并决定发送一个取消交易时，状态会从 TxpoolBlocked 变为 TxpoolCancelPending。
	// 这个状态表示系统正在等待取消交易被确认，以解除交易池的阻塞状态。
	TxpoolCancelPending
)

// 主循环，定期检查和处理交易池状态，加载区块并发布状态到 L1。
// 启动接收处理循环，定期检查交易池状态，加载区块并发布状态。
// loop 方法是 BatchSubmitter 的核心循环，负责定期检查和处理交易池状态，加载区块并发布状态到 L1。
func (l *BatchSubmitter) loop() {
	// 负责定期检查和处理交易池状态，加载区块并发布状态到 L1。
	defer l.wg.Done()

	// 创建接收通道，用于接收交易回执
	receiptsCh := make(chan txmgr.TxReceipt[txRef])
	// 初始化交易队列
	queue := txmgr.NewQueue[txRef](l.killCtx, l.Txmgr, l.Config.MaxPendingTransactions)
	// 创建错误组，用于管理并发的 DA 请求
	daGroup := &errgroup.Group{}
	// errgroup with limit of 0 means no goroutine is able to run concurrently,
	// so we only set the limit if it is greater than 0.
	// errgroup 的限制为 0 意味着没有 goroutine 能够并发运行，
	// 因此我们仅当限制大于 0 时才设置限制。
	if l.Config.MaxConcurrentDARequests > 0 {
		daGroup.SetLimit(int(l.Config.MaxConcurrentDARequests))
	}

	// start the receipt/result processing loop
	// 启动接收处理循环
	receiptLoopDone := make(chan struct{})
	// 关闭接收循环
	defer close(receiptLoopDone) // shut down receipt loop

	// 初始化交易池状态
	l.txpoolMutex.Lock()
	l.txpoolState = TxpoolGood
	l.txpoolMutex.Unlock()

	// 启动 goroutine 处理接收到的交易回执
	go func() {
		for {
			select {
			// 处理 receiptsCh 交易回执，更新交易池状态
			case r := <-receiptsCh:
				l.txpoolMutex.Lock()
				// 检查是否有不兼容的交易阻塞了交易池
				if errors.Is(r.Err, txpool.ErrAlreadyReserved) && l.txpoolState == TxpoolGood {
					l.txpoolState = TxpoolBlocked
					l.txpoolBlockedBlob = r.ID.isBlob
					l.Log.Info("incompatible tx in txpool", "is_blob", r.ID.isBlob)
				} else if r.ID.isCancel && l.txpoolState == TxpoolCancelPending {
					// Set state to TxpoolGood even if the cancellation transaction ended in error
					// since the stuck transaction could have cleared while we were waiting.
					// 取消交易完成，重置交易池状态
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

	// 创建定时器，用于定期执行主循环
	ticker := time.NewTicker(l.Config.PollInterval)
	defer ticker.Stop()

	// 将交易发布到L1 以及 DA 中，，检查返回值
	publishAndWait := func() {
		l.publishStateToL1(queue, receiptsCh, daGroup)
		if !l.Txmgr.IsClosed() {
			if l.Config.UseAltDA {
				// 等待替代 DA 写入完成
				l.Log.Info("Waiting for altDA writes to complete...")
				err := daGroup.Wait()
				if err != nil {
					l.Log.Error("Error returned by one of the altda goroutines waited on", "err", err)
				}
			}
			// 等待 L1 交易确认
			l.Log.Info("Waiting for L1 txs to be confirmed...")
			err := queue.Wait()
			if err != nil {
				l.Log.Error("Error returned by one of the txmgr goroutines waited on", "err", err)
			}
		} else {
			l.Log.Info("Txmgr is closed, remaining channel data won't be sent")
		}
	}

	// 主循环
	for {
		select {
		case <-ticker.C:
			// 检查交易池状态
			if !l.checkTxpool(queue, receiptsCh) {
				continue
			}
			// 加载新区块到状态
			if err := l.loadBlocksIntoState(l.shutdownCtx); errors.Is(err, ErrReorg) {
				// 处理重组情况
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
				// 发布所有待处理状态并等待结果
				publishAndWait()
				l.clearState(l.shutdownCtx)
				continue
			}
			// 发布状态到 L1
			l.publishStateToL1(queue, receiptsCh, daGroup)
		case <-l.shutdownCtx.Done():
			// 处理关闭情况
			if l.Txmgr.IsClosed() {
				l.Log.Info("Txmgr is closed, remaining channel data won't be sent")
				return
			}
			// This removes any never-submitted pending channels, so these do not have to be drained with transactions.
			// Any remaining unfinished channel is terminated, so its data gets submitted.
			// 关闭通道管理器
			err := l.state.Close()
			if err != nil {
				if errors.Is(err, ErrPendingAfterClose) {
					l.Log.Warn("Closed channel manager on shutdown with pending channel(s) remaining - submitting")
				} else {
					l.Log.Error("Error closing the channel manager on shutdown", "err", err)
				}
			}
			// 发布所有剩余的通道数据
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
// publishStateToL1 将所有待处理的 TxData 排队以发布到 L1
// 当没有更多数据排队发布或排队数据时出现错误时返回
func (l *BatchSubmitter) publishStateToL1(queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef], daGroup *errgroup.Group) {
	// 无限循环,持续尝试发布交易到 L1
	for {
		// if the txmgr is closed, we stop the transaction sending
		// 检查交易管理器是否已关闭
		if l.Txmgr.IsClosed() {
			l.Log.Info("Txmgr is closed, aborting state publishing")
			return
		}
		// 检查交易池状态是否正常
		if !l.checkTxpool(queue, receiptsCh) {
			l.Log.Info("txpool state is not good, aborting state publishing")
			return
		}
		// 尝试发布一个交易到 L1
		err := l.publishTxToL1(l.killCtx, queue, receiptsCh, daGroup)
		if err != nil {
			// 如果错误不是 EOF(表示没有更多数据),则记录错误
			if err != io.EOF {
				l.Log.Error("Error publishing tx to l1", "err", err)
			}
			// 无论是什么错误,都停止发布过程
			return
		}
		// 如果成功发布,继续循环尝试发布下一个交易
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
	// 1. 获取 L1 链的最新区块信息
	l1tip, err := l.l1Tip(ctx)
	if err != nil {
		l.Log.Error("Failed to query L1 tip", "err", err)
		return err
	}
	// 2. 记录 L1 最新区块信息
	l.recordL1Tip(l1tip)

	// Collect next transaction data. This pulls data out of the channel, so we need to make sure
	// to put it back if ever da or txmgr requests fail, by calling l.recordFailedDARequest/recordFailedTx.
	// 3. 从状态中获取下一个要发送的交易数据
	// 注意: 这会从通道中提取数据,如果后续 DA 或 txmgr 请求失败,需要确保将数据放回
	txdata, err := l.state.TxData(l1tip.ID())

	// 4. 处理获取交易数据的结果
	if err == io.EOF {
		// 没有可用的交易数据
		l.Log.Trace("No transaction data available")
		return err
	} else if err != nil {
		// 获取交易数据时发生错误
		l.Log.Error("Unable to get tx data", "err", err)
		return err
	}

	// 5. 发送交易
	if err = l.sendTransaction(txdata, queue, receiptsCh, daGroup); err != nil {
		return fmt.Errorf("BatchSubmitter.sendTransaction failed: %w", err)
	}

	// 6. 如果成功发送交易,返回 nil
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
// publishToAltDAAndL1 将 交易数据 发布到替代数据可用性(DA)提供商,然后将承诺发送到 L1
func (l *BatchSubmitter) publishToAltDAAndL1(txdata txData, queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef], daGroup *errgroup.Group) {
	// sanity checks
	// 1. 进行安全性检查
	// 确保交易数据只包含一个帧
	if nf := len(txdata.frames); nf != 1 {
		l.Log.Crit("Unexpected number of frames in calldata tx", "num_frames", nf)
	}
	// 确保交易数据不是 blob 类型
	if txdata.asBlob {
		l.Log.Crit("Unexpected blob txdata with AltDA enabled")
	}

	// when posting txdata to an external DA Provider, we use a goroutine to avoid blocking the main loop
	// since it may take a while for the request to return.
	// 2. 使用 errgroup 启动一个 goroutine 来处理 DA 发布
	// 这是为了避免阻塞主循环,因为 DA 请求可能需要一些时间
	goroutineSpawned := daGroup.TryGo(func() error {
		// TODO: probably shouldn't be using the global shutdownCtx here, see https://go.dev/blog/context-and-structs
		// but sendTransaction receives l.killCtx as an argument, which currently is only canceled after waiting for the main loop
		// to exit, which would wait on this DA call to finish, which would take a long time.
		// So we prefer to mimic the behavior of txmgr and cancel all pending DA/txmgr requests when the batcher is stopped.
		// 3. 将交易数据发送到替代 DA 提供商
		comm, err := l.AltDA.SetInput(l.shutdownCtx, txdata.CallData())
		if err != nil {
			// 3.1 如果发送失败,记录错误并重新排队
			l.Log.Error("Failed to post input to Alt DA", "error", err)
			// requeue frame if we fail to post to the DA Provider so it can be retried
			// note: this assumes that the da server caches requests, otherwise it might lead to resubmissions of the blobs
			l.recordFailedDARequest(txdata.ID(), err)
			return nil
		}
		// 4. 记录成功发送到 DA 提供商的日志
		l.Log.Info("Set altda input", "commitment", comm, "tx", txdata.ID())
		// 5. 创建包含 DA 承诺的交易候选
		candidate := l.calldataTxCandidate(comm.TxData())
		// 6. 发送包含 DA 承诺的交易到 L1
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
// sendTransaction 创建并排队发送一个交易到批处理收件箱地址
// 如果 txmgr 队列达到最大待处理限制,此调用将阻塞
func (l *BatchSubmitter) sendTransaction(txdata txData, queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef], daGroup *errgroup.Group) error {
	var err error

	// if Alt DA is enabled we post the txdata to the DA Provider and replace it with the commitment.
	// 1. 检查是否使用替代数据可用性(Alt DA)
	if l.Config.UseAltDA {
		// 如果启用了 Alt DA,将交易数据发布到 DA 提供商并替换为承诺
		l.publishToAltDAAndL1(txdata, queue, receiptsCh, daGroup)
		// 返回 nil 以允许 publishStateToL1 继续处理下一个 txdata
		// we return nil to allow publishStateToL1 to keep processing the next txdata
		return nil
	}

	var candidate *txmgr.TxCandidate
	// 2. 根据交易类型创建交易候选
	if txdata.asBlob {
		// 2.1 如果是 blob 类型交易
		if candidate, err = l.blobTxCandidate(txdata); err != nil {
			// We could potentially fall through and try a calldata tx instead, but this would
			// likely result in the chain spending more in gas fees than it is tuned for, so best
			// to just fail. We do not expect this error to trigger unless there is a serious bug
			// or configuration issue.
			// 如果创建 blob 交易候选失败,返回错误
			return fmt.Errorf("could not create blob tx candidate: %w", err)
		}
	} else {
		// sanity check
		// 2.2 如果是普通交易
		// 进行安全检查,确保只有一个帧
		if nf := len(txdata.frames); nf != 1 {
			l.Log.Crit("Unexpected number of frames in calldata tx", "num_frames", nf)
		}
		// 创建普通交易候选
		candidate = l.calldataTxCandidate(txdata.CallData())
	}
	// 3. 发送交易
	l.sendTx(txdata, false, candidate, queue, receiptsCh)
	return nil
}

// sendTx uses the txmgr queue to send the given transaction candidate after setting its
// gaslimit. It will block if the txmgr queue has reached its MaxPendingTransactions limit.
// sendTx 在设置其 gaslimit 后使用 txmgr 队列发送给定的交易候选。如果 txmgr 队列已达到其 MaxPendingTransactions 限制，它将被阻止。
func (l *BatchSubmitter) sendTx(txdata txData, isCancel bool, candidate *txmgr.TxCandidate, queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef]) {
	// IntrinsicGas 计算给定数据的 'intrinsic gas'(固有gas消耗)
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

// handleReceipt 处理交易回执，根据交易的执行结果更新交易状态
func (l *BatchSubmitter) handleReceipt(r txmgr.TxReceipt[txRef]) {
	// Record TX Status
	// 检查交易是否有错误
	if r.Err != nil {
		// 如果交易执行出错，记录失败的交易
		l.recordFailedTx(r.ID.id, r.Err)
	} else {
		// 如果交易执行成功，记录确认的交易
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

// recordFailedTx 记录失败的交易
func (l *BatchSubmitter) recordFailedTx(id txID, err error) {
	// 1. 使用 Warning 级别记录交易失败的日志
	// logFields 函数用于格式化日志字段,包含交易ID和错误信息
	l.Log.Warn("Transaction failed to send", logFields(id, err)...)
	// 2. 调用 state 的 TxFailed 方法,更新交易状态为失败
	l.state.TxFailed(id)
}

// recordConfirmedTx 记录已确认的交易
func (l *BatchSubmitter) recordConfirmedTx(id txID, receipt *types.Receipt) {
	// 1. 记录交易确认的日志信息
	// 使用 Info 级别记录日志,表示这是一个正常的操作
	// logFields 函数用于格式化日志字段,包含交易ID和交易回执
	l.Log.Info("Transaction confirmed", logFields(id, receipt)...)
	// 2. 从交易回执中提取 L1 区块信息
	// eth.ReceiptBlockID 函数从交易回执中获取相应的 L1 区块 ID
	l1block := eth.ReceiptBlockID(receipt)
	// 3. 更新内部状态
	// 调用 state 的 TxConfirmed 方法,更新交易状态为已确认
	// 传入交易 ID 和 L1 区块信息,用于更新通道状态和记录确认区块
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

// checkTxpool 检查交易池的状态,并在必要时采取相应的行动
func (l *BatchSubmitter) checkTxpool(queue *txmgr.Queue[txRef], receiptsCh chan txmgr.TxReceipt[txRef]) bool {
	l.txpoolMutex.Lock()
	// 2. 检查交易池是否处于阻塞状态
	if l.txpoolState == TxpoolBlocked {
		// txpoolState is set to Blocked only if Send() is returning
		// ErrAlreadyReserved. In this case, the TxMgr nonce should be reset to nil,
		// allowing us to send a cancellation transaction.
		// txpoolState 设置为 Blocked 只有在 Send() 返回 ErrAlreadyReserved 时才会发生。
		// 在这种情况下,应该将 TxMgr 的 nonce 重置为 nil,
		// 允许我们发送一个取消交易。

		// 3. 将交易池状态更新为等待取消
		l.txpoolState = TxpoolCancelPending
		// 4. 记录阻塞交易是否为 blob 类型
		isBlob := l.txpoolBlockedBlob
		// 5. 解锁互斥锁
		l.txpoolMutex.Unlock()
		// 6. 发送取消阻塞交易的请求
		l.cancelBlockingTx(queue, receiptsCh, isBlob)
		// 7. 返回 false,表示交易池不处于良好状态
		return false
	}
	// 8. 如果交易池状态为 TxpoolGood,则记录结果
	r := l.txpoolState == TxpoolGood
	// 9. 解锁互斥锁
	l.txpoolMutex.Unlock()
	// 10. 返回交易池状态是否良好
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
