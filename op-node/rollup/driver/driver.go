package driver

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"

	altda "github.com/ethereum-optimism/optimism/op-alt-da"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/async"
	"github.com/ethereum-optimism/optimism/op-node/rollup/attributes"
	"github.com/ethereum-optimism/optimism/op-node/rollup/clsync"
	"github.com/ethereum-optimism/optimism/op-node/rollup/conductor"
	"github.com/ethereum-optimism/optimism/op-node/rollup/confdepth"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-node/rollup/engine"
	"github.com/ethereum-optimism/optimism/op-node/rollup/event"
	"github.com/ethereum-optimism/optimism/op-node/rollup/finality"
	"github.com/ethereum-optimism/optimism/op-node/rollup/interop"
	"github.com/ethereum-optimism/optimism/op-node/rollup/sequencing"
	"github.com/ethereum-optimism/optimism/op-node/rollup/status"
	"github.com/ethereum-optimism/optimism/op-node/rollup/sync"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// aliases to not disrupt op-conductor code
var (
	ErrSequencerAlreadyStarted = sequencing.ErrSequencerAlreadyStarted
	ErrSequencerAlreadyStopped = sequencing.ErrSequencerAlreadyStopped
)

type Metrics interface {
	RecordPipelineReset()
	RecordPublishingError()
	RecordDerivationError()

	RecordReceivedUnsafePayload(payload *eth.ExecutionPayloadEnvelope)

	RecordL1Ref(name string, ref eth.L1BlockRef)
	RecordL2Ref(name string, ref eth.L2BlockRef)
	RecordChannelInputBytes(inputCompressedBytes int)
	RecordHeadChannelOpened()
	RecordChannelTimedOut()
	RecordFrame()

	RecordDerivedBatches(batchType string)

	RecordUnsafePayloadsBuffer(length uint64, memSize uint64, next eth.BlockID)

	SetDerivationIdle(idle bool)

	RecordL1ReorgDepth(d uint64)

	engine.Metrics
	L1FetcherMetrics
	event.Metrics
	sequencing.Metrics
}

type L1Chain interface {
	derive.L1Fetcher
	L1BlockRefByLabel(context.Context, eth.BlockLabel) (eth.L1BlockRef, error)
}

type L2Chain interface {
	engine.Engine
	L2BlockRefByLabel(ctx context.Context, label eth.BlockLabel) (eth.L2BlockRef, error)
	L2BlockRefByHash(ctx context.Context, l2Hash common.Hash) (eth.L2BlockRef, error)
	L2BlockRefByNumber(ctx context.Context, num uint64) (eth.L2BlockRef, error)
}

type DerivationPipeline interface {
	Reset()
	Step(ctx context.Context, pendingSafeHead eth.L2BlockRef) (*derive.AttributesWithParent, error)
	Origin() eth.L1BlockRef
	DerivationReady() bool
	ConfirmEngineReset()
}

type EngineController interface {
	engine.LocalEngineControl
	// IsEngineSyncing 检查引擎是否正在同步。
	IsEngineSyncing() bool
	// InsertUnsafePayload 插入一个不安全的执行载荷。
	// InsertUnsafePayload 在 Optimism 的上下文中，这通常是一个新的 L2 区块。
	InsertUnsafePayload(ctx context.Context, payload *eth.ExecutionPayloadEnvelope, ref eth.L2BlockRef) error
	// TryUpdateEngine 尝试更新引擎状态。
	TryUpdateEngine(ctx context.Context) error
	// TryBackupUnsafeReorg 尝试执行不安全的重组备份。
	TryBackupUnsafeReorg(ctx context.Context) (bool, error)
}

type CLSync interface {
	LowestQueuedUnsafeBlock() eth.L2BlockRef
}

type AttributesHandler interface {
	// HasAttributes returns if there are any block attributes to process.
	// HasAttributes is for EngineQueue testing only, and can be removed when attribute processing is fully independent.
	HasAttributes() bool
	// SetAttributes overwrites the set of attributes. This may be nil, to clear what may be processed next.
	SetAttributes(attributes *derive.AttributesWithParent)
	// Proceed runs one attempt of processing attributes, if any.
	// Proceed returns io.EOF if there are no attributes to process.
	Proceed(ctx context.Context) error
}

type Finalizer interface {
	FinalizedL1() eth.L1BlockRef
	event.Deriver
}

type AltDAIface interface {
	// Notify L1 finalized head so AltDA finality is always behind L1
	Finalize(ref eth.L1BlockRef)
	// Set the engine finalization signal callback
	OnFinalizedHeadSignal(f altda.HeadSignalFn)

	derive.AltDAInputFetcher
}

type SyncStatusTracker interface {
	event.Deriver
	SyncStatus() *eth.SyncStatus
	L1Head() eth.L1BlockRef
}

type Network interface {
	// PublishL2Payload is called by the driver whenever there is a new payload to publish, synchronously with the driver main loop.
	PublishL2Payload(ctx context.Context, payload *eth.ExecutionPayloadEnvelope) error
}

type AltSync interface {
	// RequestL2Range informs the sync source that the given range of L2 blocks is missing,
	// and should be retrieved from any available alternative syncing source.
	// The start and end of the range are exclusive:
	// the start is the head we already have, the end is the first thing we have queued up.
	// It's the task of the alt-sync mechanism to use this hint to fetch the right payloads.
	// Note that the end and start may not be consistent: in this case the sync method should fetch older history
	//
	// If the end value is zeroed, then the sync-method may determine the end free of choice,
	// e.g. sync till the chain head meets the wallclock time. This functionality is optional:
	// a fixed target to sync towards may be determined by picking up payloads through P2P gossip or other sources.
	//
	// The sync results should be returned back to the driver via the OnUnsafeL2Payload(ctx, payload) method.
	// The latest requested range should always take priority over previous requests.
	// There may be overlaps in requested ranges.
	// An error may be returned if the scheduling fails immediately, e.g. a context timeout.
	RequestL2Range(ctx context.Context, start, end eth.L2BlockRef) error
}

type SequencerStateListener interface {
	SequencerStarted() error
	SequencerStopped() error
}

type Drain interface {
	Drain() error
}

// NewDriver composes an events handler that tracks L1 state, triggers L2 Derivation, and optionally sequences new L2 blocks.
func NewDriver(
	sys event.Registry,
	drain Drain,
	driverCfg *Config,
	cfg *rollup.Config,
	l2 L2Chain,
	l1 L1Chain,
	supervisor interop.InteropBackend, // may be nil pre-interop.
	l1Blobs derive.L1BlobsFetcher,
	altSync AltSync,
	network Network,
	log log.Logger,
	metrics Metrics,
	sequencerStateListener sequencing.SequencerStateListener,
	safeHeadListener rollup.SafeHeadListener,
	syncCfg *sync.Config,
	sequencerConductor conductor.SequencerConductor,
	altDA AltDAIface,
) *Driver {
	driverCtx, driverCancel := context.WithCancel(context.Background())
	// 设置事件注册的默认选项
	opts := event.DefaultRegisterOpts()

	// If interop is scheduled we start the driver.
	// It will then be ready to pick up verification work
	// as soon as we reach the upgrade time (if the upgrade is not already active).
	// 如果计划进行互操作，我们将启动驱动程序。
	// 然后，一旦我们到达升级时间（如果升级尚未启动），它就会准备好开始验证工作。
	// 如果配置了互操作时间,注册互操作派生器
	if cfg.InteropTime != nil {
		// 互操作性派生器(Interoperability Deriver)的函数。这个组件在系统升级到新版本时起着关键作用,特别是在处理跨版本兼容性时。
		interopDeriver := interop.NewInteropDeriver(log, cfg, driverCtx, supervisor, l2)
		sys.Register("interop", interopDeriver, opts)
	}
	// 创建并注册状态跟踪器
	// 用于创建一个状态跟踪器(Status Tracker)。这是一个重要的系统组件,主要负责监控和记录整个系统的运行状态。
	statusTracker := status.NewStatusTracker(log, metrics)
	sys.Register("status", statusTracker, opts)
	// 创建并注册 L1 跟踪器
	// 创建一个 L1 跟踪器(L1 Tracker),然后将其注册到系统中。这个组件专门负责跟踪以太坊主网(L1)的状态。
	// 持续跟踪 L1 链的最新区块,包括区块号、哈希值、时间戳等信息。
	// 当有新的 L1 区块产生时,更新系统中的 L1 状态信息。
	// 检测并处理 L1 链上可能发生的重组事件,确保系统始终跟随最长的有效链。
	// 为其他系统组件提供查询 L1 状态的接口,如获取特定高度的区块信息。
	// 当 L1 状态发生重要变化时(如新区块、重组等),通知其他相关组件。
	l1Tracker := status.NewL1Tracker(l1)
	sys.Register("l1-blocks", l1Tracker, opts)
	// 创建带度量的 L1 获取器
	// 在原有的 L1 数据获取功能基础上增加了性能监控和度量统计的能力。
	// 可以记录诸如 L1 数据获取的频率、延迟、成功率等指标。
	// 能够统计和记录在获取 L1 数据过程中遇到的各种错误。
	l1 = NewMeteredL1Fetcher(l1Tracker, metrics)
	// 用于创建一个确认深度(Confirmation Depth)管理器，这个组件在区块链系统中扮演着重要角色,主要用于处理区块确认的逻辑。
	// 使用 statusTracker.L1Head 来获取当前 L1 链的头部信息。
	// 确认深度管理对于确保从 L1 到 L2 的安全状态转移非常重要。它帮助系统决定何时可以安全地基于 L1 的状态进行 L2 的操作,从而保证整个系统的安全性和可靠性。
	verifConfDepth := confdepth.NewConfDepth(driverCfg.VerifierConfDepth, statusTracker.L1Head, l1)
	// 创建并注册引擎控制器
	// 用于创建一个引擎控制器(Engine Controller)。这是系统中的一个核心组件,负责管理和控制执行引擎(通常是 L2 链的执行客户端)。
	// 它接收 l2 参数,这是 L2 链的接口,用于与执行引擎进行交互。
	// 管理 L2 链的状态同步过程,确保本地状态与网络一致。
	// 控制新区块的处理流程,包括验证、执行和应用状态变更。
	// 管理待处理交易池,决定哪些交易应该被包含在下一个区块中。
	// 与共识机制协调,确保区块的生成和确认符合协议规则。
	// 在出现分叉时,决定遵循哪条链。
	// 监控和优化引擎的性能,如调整资源分配、优化数据存储等。
	// 处理执行过程中可能出现的各种错误和异常情况。
	// 为其他系统组件提供访问和控制执行引擎的 API。
	ec := engine.NewEngineController(l2, log, metrics, cfg, syncCfg,
		sys.Register("engine-controller", nil, opts))
	// 创建一个引擎重置派生器（Engine Reset Deriver）。这个组件在系统中扮演着特殊的角色，主要负责处理引擎重置的逻辑。
	// 在重置过程中，确保 L2 状态与 L1 状态保持一致。
	// 在重置时，清理可能存在的无效或过时的数据。
	// 协调引擎重启过程，确保所有相关组件都正确重新初始化。
	// 在重置过程中出现错误时，实施恢复策略。
	// 向其他系统组件报告重置的进度和状态。
	// 在重置完成后，执行必要的安全检查，确保系统处于一致和安全的状态。
	sys.Register("engine-reset",
		engine.NewEngineResetDeriver(driverCtx, log, cfg, l1, l2, syncCfg), opts)
	// 创建并注册 CL 同步器，Consensus Layer
	// CL 同步器主要负责处理与共识层相关的同步任务。
	// 确保本地节点的共识状态与网络的其余部分保持一致。
	// 同步最新的区块头信息,这对于验证新区块和交易至关重要。
	// 如果适用,管理和更新当前的验证者集合。
	// 在出现分叉时,帮助确定应该遵循哪个分叉。
	// 跟踪网络的安全头部,即已经被足够多验证者确认的最新区块。
	// 处理与区块最终确定性相关的逻辑。
	// 提供关于节点同步状态的信息,如是否已完全同步、当前同步进度等。
	// 可能包括对轻客户端同步协议的支持。
	clSync := clsync.NewCLSync(log, cfg, metrics) // alt-sync still uses cl-sync state to determine what to sync to
	sys.Register("cl-sync", clSync, opts)
	// 根据配置创建并注册终结器
	var finalizer Finalizer
	if cfg.AltDAEnabled() {
		// 用于启用了替代数据可用性解决方案的 Optimism 设置。
		// 与替代数据可用性层(altDA)交互。
		// 可能涉及额外的数据可用性确认步骤。
		// 在确定 L2 区块最终性时,考虑替代数据可用性层的状态。
		finalizer = finality.NewAltDAFinalizer(driverCtx, log, cfg, l1, altDA)
	} else {
		// 主要依赖 L1 (以太坊主网) 来确定 L2 区块的最终性。
		// 跟踪 L1 的最终确认区块。
		// 基于 L1 的最终性来确定 L2 区块的最终性。
		// 管理 L2 的安全头和最终确认头。
		finalizer = finality.NewFinalizer(driverCtx, log, cfg, l1)
	}
	sys.Register("finalizer", finalizer, opts)
	// 主要负责处理区块属性。
	// 解析和验证新区块的属性信息。
	// 确保区块属性符合预定义的规则和协议要求。
	// 将验证过的属性应用到区块上，可能影响区块的处理方式。
	// 处理一些特殊的属性，如系统升级标志、协议变更等。
	// 在出现属性冲突时，决定如何处理和解决这些冲突。
	// 确保重要的属性信息能够正确地传播到系统的其他部分。
	// 执行与属性相关的安全检查，防止潜在的攻击或滥用。
	sys.Register("attributes-handler",
		attributes.NewAttributesHandler(log, cfg, driverCtx, l2), opts)
	// 创建并注册派生管道 负责从 L1 数据派生 L2 状态
	// 从 L1 链读取相关数据，包括交易、状态更新等。
	// 解析从 L1 获取的数据，提取出需要在 L2 上执行的信息。
	// 基于 L1 数据计算 L2 的状态变化。
	// 确定 L2 交易的执行顺序。
	// 生成新的 L2 区块，包括设置区块头、打包交易等。
	// 验证派生的 L2 数据的正确性和一致性。
	// 处理派生过程中可能出现的各种错误和异常情况。
	// 优化派生过程，提高处理速度和效率。
	// 执行必要的安全检查，确保派生的数据不会引入漏洞。
	derivationPipeline := derive.NewDerivationPipeline(log, cfg, verifConfDepth, l1Blobs, altDA, l2, metrics)
	sys.Register("pipeline",
		derive.NewPipelineDeriver(driverCtx, derivationPipeline), opts)

	syncDeriver := &SyncDeriver{
		Derivation:     derivationPipeline,
		SafeHeadNotifs: safeHeadListener,
		CLSync:         clSync,
		Engine:         ec,
		SyncCfg:        syncCfg,
		Config:         cfg,
		L1:             l1,
		L2:             l2,
		Log:            log,
		Ctx:            driverCtx,
		Drain:          drain.Drain,
	}
	sys.Register("sync", syncDeriver, opts)
	// 这个组件在 Optimism 系统中扮演着重要角色，主要负责管理和控制执行引擎的派生过程。
	// 协调从 L1 数据派生 L2 状态的过程。
	// 管理 L2 区块的执行过程，包括交易的应用和状态更新。
	// 确保引擎的执行结果与网络共识保持一致。
	// 在出现分叉时，管理引擎的行为，确保选择正确的分支。
	// 监控和优化引擎的执行性能。
	// 处理执行过程中可能出现的错误，并实施恢复策略。
	// 管理引擎的状态同步过程，确保与网络其他部分保持一致。
	// 执行必要的安全检查，确保引擎的执行不会引入漏洞。
	// 为系统的其他部分提供访问和控制执行引擎的统一接口。
	sys.Register("engine", engine.NewEngDeriver(log, driverCtx, cfg, metrics, ec), opts)
	// 负责管理和调度派生过程的各个步骤
	// 定义派生过程中的各个步骤，如数据获取、状态计算、区块生成等。
	// 实现调度策略，决定何时执行哪些步骤。
	// 可能支持某些步骤的并行执行，以提高效率。
	// 管理步骤之间的依赖关系，确保按正确的顺序执行。
	// 根据系统负载和各步骤的重要性分配计算资源。
	// 处理执行过程中的错误，并决定是否需要重试或跳过某些步骤。
	// 跟踪整个派生过程的进度，提供状态报告。
	// 根据实时情况动态调整调度策略，以优化性能。
	// 支持派生过程的中断和恢复，以应对系统重启或其他中断情况。
	schedDeriv := NewStepSchedulingDeriver(log)
	sys.Register("step-scheduler", schedDeriv, opts)
	// 如果启用了排序器,创建并注册排序器
	// 代码检查 driverCfg.SequencerEnabled 是否为真，即是否启用了排序器功能。
	var sequencer sequencing.SequencerIface
	if driverCfg.SequencerEnabled {
		// 创建异步gossip组件：
		// 这个组件负责异步地广播交易和区块信息到网络中的其他节点。
		asyncGossiper := async.NewAsyncGossiper(driverCtx, network, log, metrics)
		// 这个组件负责构建区块属性，可能包括从L1和L2链获取必要的信息。
		attrBuilder := derive.NewFetchingAttributesBuilder(cfg, l1, l2)
		// 创建排序器确认深度管理器：
		// 用于确定排序器在处理L1区块时需要等待的确认深度。
		sequencerConfDepth := confdepth.NewConfDepth(driverCfg.SequencerConfDepth, statusTracker.L1Head, l1)
		// L1源选择器：
		// 这个组件负责为每个L2区块选择合适的L1源区块。
		findL1Origin := sequencing.NewL1OriginSelector(log, cfg, sequencerConfDepth)
		// 创建了主要的排序器实例，它整合了前面创建的所有组件。
		sequencer = sequencing.NewSequencer(driverCtx, log, cfg, attrBuilder, findL1Origin,
			sequencerStateListener, sequencerConductor, asyncGossiper, metrics)
		// 将创建的排序器注册到系统中，使其能够被其他组件访问和使用。
		sys.Register("sequencer", sequencer, opts)
	} else {
		// 如果排序器未启用，则创建一个禁用的排序器实例：
		sequencer = sequencing.DisabledSequencer{}
	}
	driverEmitter := sys.Register("driver", nil, opts)
	// 创建 Driver 实例
	driver := &Driver{
		statusTracker:    statusTracker,
		SyncDeriver:      syncDeriver,
		sched:            schedDeriv,
		emitter:          driverEmitter,
		drain:            drain.Drain,
		stateReq:         make(chan chan struct{}),
		forceReset:       make(chan chan struct{}, 10),
		driverConfig:     driverCfg,
		driverCtx:        driverCtx,
		driverCancel:     driverCancel,
		log:              log,
		sequencer:        sequencer,
		network:          network,
		metrics:          metrics,
		l1HeadSig:        make(chan eth.L1BlockRef, 10),
		l1SafeSig:        make(chan eth.L1BlockRef, 10),
		l1FinalizedSig:   make(chan eth.L1BlockRef, 10),
		unsafeL2Payloads: make(chan *eth.ExecutionPayloadEnvelope, 10),
		altSync:          altSync,
	}

	return driver
}
