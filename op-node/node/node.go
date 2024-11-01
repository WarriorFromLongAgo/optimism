package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ethereum/go-ethereum"
	gethevent "github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"

	altda "github.com/ethereum-optimism/optimism/op-alt-da"
	"github.com/ethereum-optimism/optimism/op-node/metrics"
	"github.com/ethereum-optimism/optimism/op-node/node/safedb"
	"github.com/ethereum-optimism/optimism/op-node/p2p"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/conductor"
	"github.com/ethereum-optimism/optimism/op-node/rollup/driver"
	"github.com/ethereum-optimism/optimism/op-node/rollup/event"
	"github.com/ethereum-optimism/optimism/op-node/rollup/sequencing"
	"github.com/ethereum-optimism/optimism/op-node/rollup/sync"
	"github.com/ethereum-optimism/optimism/op-service/client"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/httputil"
	"github.com/ethereum-optimism/optimism/op-service/oppprof"
	"github.com/ethereum-optimism/optimism/op-service/retry"
	"github.com/ethereum-optimism/optimism/op-service/sources"
)

// 它实现了 Optimism 节点的核心功能

var ErrAlreadyClosed = errors.New("node is already closed")

type closableSafeDB interface {
	rollup.SafeHeadListener
	SafeDBReader
	io.Closer
}

type OpNode struct {
	// Retain the config to test for active features rather than test for runtime state.
	cfg        *Config
	log        log.Logger
	appVersion string
	metrics    *metrics.Metrics

	l1HeadsSub     ethereum.Subscription // Subscription to get L1 heads (automatically re-subscribes on error)
	l1SafeSub      ethereum.Subscription // Subscription to get L1 safe blocks, a.k.a. justified data (polling)
	l1FinalizedSub ethereum.Subscription // Subscription to get L1 safe blocks, a.k.a. justified data (polling)

	eventSys   event.System
	eventDrain event.Drainer

	l1Source  *sources.L1Client     // L1 Client to fetch data from
	l2Driver  *driver.Driver        // L2 Engine to Sync
	l2Source  *sources.EngineClient // L2 Execution Engine RPC bindings
	server    *rpcServer            // RPC server hosting the rollup-node API
	p2pNode   *p2p.NodeP2P          // P2P node functionality
	p2pSigner p2p.Signer            // p2p gossip application messages will be signed with this signer
	tracer    Tracer                // tracer to get events for testing/debugging
	runCfg    *RuntimeConfig        // runtime configurables

	safeDB closableSafeDB

	rollupHalt string // when to halt the rollup, disabled if empty

	pprofService *oppprof.Service
	metricsSrv   *httputil.HTTPServer

	beacon *sources.L1BeaconClient

	supervisor *sources.SupervisorClient

	// some resources cannot be stopped directly, like the p2p gossipsub router (not our design),
	// and depend on this ctx to be closed.
	resourcesCtx   context.Context
	resourcesClose context.CancelFunc

	// Indicates when it's safe to close data sources used by the runtimeConfig bg loader
	runtimeConfigReloaderDone chan struct{}

	closed atomic.Bool

	// cancels execution prematurely, e.g. to halt. This may be nil.
	cancel context.CancelCauseFunc
	halted atomic.Bool
}

// The OpNode handles incoming gossip
var _ p2p.GossipIn = (*OpNode)(nil)

// New creates a new OpNode instance.
// The provided ctx argument is for the span of initialization only;
// the node will immediately Stop(ctx) before finishing initialization if the context is canceled during initialization.
// New 创建一个新的 OpNode 实例。
// 提供的 ctx 参数仅用于初始化的跨度；
// 确保如果无法初始化节点，则节点将在完成初始化之前立即 Stop(ctx)。
func New(ctx context.Context, cfg *Config, log log.Logger, appVersion string, m *metrics.Metrics) (*OpNode, error) {
	if err := cfg.Check(); err != nil {
		return nil, err
	}

	n := &OpNode{
		cfg:        cfg,
		log:        log,
		appVersion: appVersion,
		metrics:    m,
		rollupHalt: cfg.RollupHalt,
		cancel:     cfg.Cancel,
	}
	// not a context leak, gossipsub is closed with a context.
	n.resourcesCtx, n.resourcesClose = context.WithCancel(context.Background())

	err := n.init(ctx, cfg)
	if err != nil {
		// 确保如果无法初始化节点，我们始终关闭节点资源。
		log.Error("Error initializing the rollup node", "err", err)
		// ensure we always close the node resources if we fail to initialize the node.
		if closeErr := n.Stop(ctx); closeErr != nil {
			return nil, multierror.Append(err, closeErr)
		}
		return nil, err
	}
	return n, nil
}

func (n *OpNode) init(ctx context.Context, cfg *Config) error {
	n.log.Info("Initializing rollup node", "version", n.appVersion)
	// 初始化追踪器 (tracer)
	if err := n.initTracer(ctx, cfg); err != nil {
		return fmt.Errorf("failed to init the trace: %w", err)
	}
	// 初始化事件系统
	n.initEventSystem()
	// 初始化 L1 相关组件
	if err := n.initL1(ctx, cfg); err != nil {
		return fmt.Errorf("failed to init L1: %w", err)
	}
	// 初始化 L1 信标链 API
	if err := n.initL1BeaconAPI(ctx, cfg); err != nil {
		return err
	}
	// 初始化 L2 相关组件
	if err := n.initL2(ctx, cfg); err != nil {
		return fmt.Errorf("failed to init L2: %w", err)
	}
	// 初始化运行时配置
	if err := n.initRuntimeConfig(ctx, cfg); err != nil { // depends on L2, to signal initial runtime values to
		return fmt.Errorf("failed to init the runtime config: %w", err)
	}
	// 初始化 P2P 签名器
	if err := n.initP2PSigner(ctx, cfg); err != nil {
		return fmt.Errorf("failed to init the P2P signer: %w", err)
	}
	// 初始化 P2P 网络栈
	if err := n.initP2P(cfg); err != nil {
		return fmt.Errorf("failed to init the P2P stack: %w", err)
	}
	// Only expose the server at the end, ensuring all RPC backend components are initialized.
	// 初始化 RPC 服务器
	if err := n.initRPCServer(cfg); err != nil {
		return fmt.Errorf("failed to init the RPC server: %w", err)
	}
	// 初始化指标服务器
	if err := n.initMetricsServer(cfg); err != nil {
		return fmt.Errorf("failed to init the metrics server: %w", err)
	}
	// 记录应用版本信息和启动状态
	n.metrics.RecordInfo(n.appVersion)
	// 初始化性能分析 (PProf) 服务
	n.metrics.RecordUp()
	if err := n.initPProf(cfg); err != nil {
		return fmt.Errorf("failed to init profiling: %w", err)
	}
	return nil
}

// initEventSystem 初始化事件系统：这个方法的主要目的是设置 OpNode 的事件系统。
func (n *OpNode) initEventSystem() {
	// This executor will be configurable in the future, for parallel event processing
	// 创建事件执行器: 这一步创建了一个同步的全局事件执行器。
	executor := event.NewGlobalSynchronous(n.resourcesCtx)
	// 创建事件系统: 使用上一步创建的执行器和节点的日志器初始化事件系统。
	sys := event.NewSystem(n.log, executor)
	// 添加指标追踪器: 为事件系统添加一个指标追踪器,用于跟踪事件相关的指标。
	sys.AddTracer(event.NewMetricsTracer(n.metrics))
	// 注册节点作为事件处理器: 将节点本身注册为事件处理器,使其能够直接处理事件。
	sys.Register("node", event.DeriverFunc(n.onEvent), event.DefaultRegisterOpts())
	// 存储事件系统: 将创建的事件系统存储在 OpNode 结构体中,以供后续使用。
	n.eventSys = sys
	// 初始化事件排空器: 创建一个事件排空器,可能用于优雅关闭,确保在系统停止前处理所有事件。
	n.eventDrain = executor
}

func (n *OpNode) initTracer(ctx context.Context, cfg *Config) error {
	if cfg.Tracer != nil {
		n.tracer = cfg.Tracer
	} else {
		n.tracer = new(noOpTracer)
	}
	return nil
}

func (n *OpNode) initL1(ctx context.Context, cfg *Config) error {
	// 1. 设置 L1 客户端
	l1Node, rpcCfg, err := cfg.L1.Setup(ctx, n.log, &cfg.Rollup)
	if err != nil {
		return fmt.Errorf("failed to get L1 RPC client: %w", err)
	}

	// 2. 创建 L1 数据源
	n.l1Source, err = sources.NewL1Client(
		client.NewInstrumentedRPC(l1Node, &n.metrics.RPCMetrics.RPCClientMetrics), n.log, n.metrics.L1SourceCache, rpcCfg)
	if err != nil {
		return fmt.Errorf("failed to create L1 source: %w", err)
	}

	// 3. 验证 L1 配置
	if err := cfg.Rollup.ValidateL1Config(ctx, n.l1Source); err != nil {
		return fmt.Errorf("failed to validate the L1 config: %w", err)
	}

	// 4. 设置 L1 订阅
	// 4.1 订阅 L1 头部更新
	// Keep subscribed to the L1 heads, which keeps the L1 maintainer pointing to the best headers to sync
	n.l1HeadsSub = gethevent.ResubscribeErr(time.Second*10, func(ctx context.Context, err error) (gethevent.Subscription, error) {
		if err != nil {
			n.log.Warn("resubscribing after failed L1 subscription", "err", err)
		}
		return eth.WatchHeadChanges(ctx, n.l1Source, n.OnNewL1Head)
	})
	// 4.2 启动错误监听协程
	go func() {
		err, ok := <-n.l1HeadsSub.Err()
		if !ok {
			return
		}
		n.log.Error("l1 heads subscription error", "err", err)
	}()

	// 4.3 设置 L1 安全区块轮询
	// Poll for the safe L1 block and finalized block,
	// which only change once per epoch at most and may be delayed.
	// 这些回调函数（OnNewL1Safe 和 OnNewL1Finalized）在收到新的 safe 或 finalized 区块时被调用，它们会将信息传递给 L2 引擎。
	n.l1SafeSub = eth.PollBlockChanges(n.log, n.l1Source, n.OnNewL1Safe, eth.Safe,
		cfg.L1EpochPollInterval, time.Second*10)
	// 4.4 设置 L1 最终确认区块轮询
	n.l1FinalizedSub = eth.PollBlockChanges(n.log, n.l1Source, n.OnNewL1Finalized, eth.Finalized,
		cfg.L1EpochPollInterval, time.Second*10)
	return nil
}

func (n *OpNode) initRuntimeConfig(ctx context.Context, cfg *Config) error {
	// attempt to load runtime config, repeat N times
	// 初始化运行时配置
	n.runCfg = NewRuntimeConfig(n.log, n.l1Source, &cfg.Rollup)
	// 定义重新加载配置的函数
	confDepth := cfg.Driver.VerifierConfDepth
	reload := func(ctx context.Context) (eth.L1BlockRef, error) {
		// 获取 L1 最新区块
		fetchCtx, fetchCancel := context.WithTimeout(ctx, time.Second*10)
		l1Head, err := n.l1Source.L1BlockRefByLabel(fetchCtx, eth.Unsafe)
		fetchCancel()
		if err != nil {
			n.log.Error("failed to fetch L1 head for runtime config initialization", "err", err)
			return eth.L1BlockRef{}, err
		}
		// 应用确认深度
		// Apply confirmation-distance
		blNum := l1Head.Number
		if blNum >= confDepth {
			blNum -= confDepth
		}
		// 获取确认的区块
		fetchCtx, fetchCancel = context.WithTimeout(ctx, time.Second*10)
		confirmed, err := n.l1Source.L1BlockRefByNumber(fetchCtx, blNum)
		fetchCancel()
		if err != nil {
			n.log.Error("failed to fetch confirmed L1 block for runtime config loading", "err", err, "number", blNum)
			return eth.L1BlockRef{}, err
		}
		// 加载运行时配置
		fetchCtx, fetchCancel = context.WithTimeout(ctx, time.Second*10)
		err = n.runCfg.Load(fetchCtx, confirmed)
		fetchCancel()
		if err != nil {
			n.log.Error("failed to fetch runtime config data", "err", err)
			return l1Head, err
		}
		// 处理协议版本更新
		err = n.handleProtocolVersionsUpdate(ctx)
		return l1Head, err
	}

	// initialize the runtime config before unblocking
	if err := retry.Do0(ctx, 5, retry.Fixed(time.Second*10), func() error {
		_, err := reload(ctx)
		if errors.Is(err, errNodeHalt) { // don't retry on halt error
			err = nil
		}
		return err
	}); err != nil {
		return fmt.Errorf("failed to load runtime configuration repeatedly, last error: %w", err)
	}

	// start a background loop, to keep reloading it at the configured reload interval
	reloader := func(ctx context.Context, reloadInterval time.Duration) {
		if reloadInterval <= 0 {
			n.log.Debug("not running runtime-config reloading background loop")
			return
		}
		ticker := time.NewTicker(reloadInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// If the reload fails, we will try again the next interval.
				// Missing a runtime-config update is not critical, and we do not want to overwhelm the L1 RPC.
				l1Head, err := reload(ctx)
				if err != nil {
					if errors.Is(err, errNodeHalt) {
						n.halted.Store(true)
						if n.cancel != nil { // node cancellation is always available when started as CLI app
							n.cancel(errNodeHalt)
							return
						} else {
							n.log.Debug("opted to halt, but cannot halt node", "l1_head", l1Head)
						}
					} else {
						n.log.Warn("failed to reload runtime config", "err", err)
					}
				} else {
					n.log.Debug("reloaded runtime config", "l1_head", l1Head)
				}
			case <-ctx.Done():
				return
			}
		}
	}

	n.runtimeConfigReloaderDone = make(chan struct{})
	// Manages the lifetime of reloader. In order to safely Close the OpNode
	go func(ctx context.Context, reloadInterval time.Duration) {
		reloader(ctx, reloadInterval)
		close(n.runtimeConfigReloaderDone)
	}(n.resourcesCtx, cfg.RuntimeConfigReloadInterval) // this keeps running after initialization
	return nil
}

// initL1BeaconAPI
// Ecotone 是 Optimism 网络的一个重要升级，它引入了对 EIP-4844 的支持。EIP-4844 是以太坊的一个提案，引入了 blob 交易，这可以显著降低 Layer 2 解决方案的成本。
// L1 信标链 API 客户端是获取 EIP-4844 blob 数据的必要组件。这些 blob 数据对于 Layer 2 网络（如 Optimism）来说是至关重要的，因为它们包含了压缩的交易数据。
func (n *OpNode) initL1BeaconAPI(ctx context.Context, cfg *Config) error {
	// If Ecotone upgrade is not scheduled yet, then there is no need for a Beacon API.
	// 如果 Ecotone 升级还未安排，则不需要 Beacon API
	if cfg.Rollup.EcotoneTime == nil {
		return nil
	}
	// Once the Ecotone upgrade is scheduled, we must have initialized the Beacon API settings.
	// 一旦 Ecotone 升级被安排，我们必须初始化 Beacon API 设置
	if cfg.Beacon == nil {
		return fmt.Errorf("missing L1 Beacon Endpoint configuration: this API is mandatory for Ecotone upgrade at t=%d", *cfg.Rollup.EcotoneTime)
	}
	// 初始化客户端，即使客户端不工作也会继续
	// 我们总是初始化一个客户端。如果客户端不工作，我们将在请求中收到错误。
	// 这样，当用户选择忽略 Beacon API 要求时，op-node 可以继续非 L1 功能。
	// We always initialize a client. We will get an error on requests if the client does not work.
	// This way the op-node can continue non-L1 functionality when the user chooses to ignore the Beacon API requirement.
	beaconClient, fallbacks, err := cfg.Beacon.Setup(ctx, n.log)
	if err != nil {
		return fmt.Errorf("failed to setup L1 Beacon API client: %w", err)
	}
	beaconCfg := sources.L1BeaconClientConfig{
		FetchAllSidecars: cfg.Beacon.ShouldFetchAllSidecars(),
	}
	// 创建 L1 信标客户端
	n.beacon = sources.NewL1BeaconClient(beaconClient, beaconCfg, fallbacks...)
	// 重试获取 Beacon API 版本，以增强对 Beacon API 连接问题的鲁棒性
	// Retry retrieval of the Beacon API version, to be more robust on startup against Beacon API connection issues.
	beaconVersion, missingEndpoint, err := retry.Do2[string, bool](ctx, 5, retry.Exponential(), func() (string, bool, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Second*10)
		defer cancel()
		beaconVersion, err := n.beacon.GetVersion(ctx)
		if err != nil {
			if errors.Is(err, client.ErrNoEndpoint) {
				return "", true, nil // don't return an error, we do not have to retry when there is a config issue.
			}
			return "", false, err
		}
		return beaconVersion, false, nil
	})
	// 处理缺失端点的情况
	if missingEndpoint {
		// 如果用户明确忽略端点要求，允许继续
		// Allow the user to continue if they explicitly ignore the requirement of the endpoint.
		if cfg.Beacon.ShouldIgnoreBeaconCheck() {
			n.log.Warn("This endpoint is required for the Ecotone upgrade, but is missing, and configured to be ignored. " +
				"The node may be unable to retrieve EIP-4844 blobs data.")
			return nil
		} else {
			// 解释为什么需要端点，以及用户如何忽略这一点
			// 如果客户端告诉我们端点未配置，
			// 然后解释我们为什么需要它，以及用户可以做什么来忽略它。
			// If the client tells us the endpoint was not configured,
			// then explain why we need it, and what the user can do to ignore this.
			n.log.Error("The Ecotone upgrade requires a L1 Beacon API endpoint, to retrieve EIP-4844 blobs data. " +
				"This can be ignored with the --l1.beacon.ignore option, " +
				"but the node may be unable to sync from L1 without this endpoint.")
			return errors.New("missing L1 Beacon API endpoint")
		}
	} else if err != nil {
		// 处理检查 Beacon API 版本失败的情况
		if cfg.Beacon.ShouldIgnoreBeaconCheck() {
			n.log.Warn("Failed to check L1 Beacon API version, but configuration ignores results. "+
				"The node may be unable to retrieve EIP-4844 blobs data.", "err", err)
			return nil
		} else {
			return fmt.Errorf("failed to check L1 Beacon API version: %w", err)
		}
	} else {
		// 成功连接到 L1 Beacon API
		n.log.Info("Connected to L1 Beacon API, ready for EIP-4844 blobs retrieval.", "version", beaconVersion)
		return nil
	}
}

// initL2 初始化 OpNode 的 L2（Layer 2）相关组件
func (n *OpNode) initL2(ctx context.Context, cfg *Config) error {
	// 设置 L2 op-geth 的 RPC 客户端
	rpcClient, rpcCfg, err := cfg.L2.Setup(ctx, n.log, &cfg.Rollup)
	if err != nil {
		return fmt.Errorf("failed to setup L2 execution-engine RPC client: %w", err)
	}
	// 创建 L2 op-geth 的 RPC 客户端
	// 发送交易到 L2 网络
	// 查询 L2 状态
	// 获取区块信息
	// 执行 L2 上的智能合约调用
	n.l2Source, err = sources.NewEngineClient(
		client.NewInstrumentedRPC(rpcClient, &n.metrics.RPCClientMetrics), n.log, n.metrics.L2SourceCache, rpcCfg,
	)
	if err != nil {
		return fmt.Errorf("failed to create Engine client: %w", err)
	}
	// 验证 L2 配置
	if err := cfg.Rollup.ValidateL2Config(ctx, n.l2Source, cfg.Sync.SyncMode == sync.ELSync); err != nil {
		return err
	}
	// 如果配置了 InteropTime，设置 supervisor 客户端
	// 与监督服务进行通信，主要用于网络升级和维护，协调网络升级，监控节点健康状况，在需要时执行特定的管理操作
	if cfg.Rollup.InteropTime != nil {
		cl, err := cfg.Supervisor.SupervisorClient(ctx, n.log)
		if err != nil {
			return fmt.Errorf("failed to setup supervisor RPC client: %w", err)
		}
		n.supervisor = cl
	}
	// 设置 sequencer conductor
	// 管理和协调排序器的行为，决定何时产生新的 L2 区块，管理交易的排序和打包，在多个排序器之间协调（如果适用）
	var sequencerConductor conductor.SequencerConductor = &conductor.NoOpConductor{}
	if cfg.ConductorEnabled {
		sequencerConductor = NewConductorClient(cfg, n.log, n.metrics)
	}
	// 设置 altDA（替代数据可用性）
	// 如果未在节点 CLI 中明确激活 altDA，则配置 + 任何错误将被忽略。
	// if altDA is not explicitly activated in the node CLI, the config + any error will be ignored.
	rpCfg, err := cfg.Rollup.GetOPAltDAConfig()
	if cfg.AltDA.Enabled && err != nil {
		return fmt.Errorf("failed to get altDA config: %w", err)
	}
	// 实现替代的数据可用性解决方案。支持不同的数据可用性策略，如数据分片或外部数据存储
	altDA := altda.NewAltDA(n.log, cfg.AltDA, rpCfg, n.metrics.AltDAMetrics)
	// 设置 SafeDB
	if cfg.SafeDBPath != "" {
		n.log.Info("Safe head database enabled", "path", cfg.SafeDBPath)
		// 存储和管理"安全头"（Safe Head）信息。
		// 保存已确认安全的 L2 区块头信息，在节点重启时提供一个可信的起点，帮助防止长程攻击
		safeDB, err := safedb.NewSafeDB(n.log, cfg.SafeDBPath)
		if err != nil {
			return fmt.Errorf("failed to create safe head database at %v: %w", cfg.SafeDBPath, err)
		}
		n.safeDB = safeDB
	} else {
		n.safeDB = safedb.Disabled
	}
	// 创建 L2 驱动器
	// 负责保持 L2 网络与 L1（以太坊主网）的同步。
	// 处理新的 L2 区块，包括验证和应用状态更新。
	// 在排序器模式下，管理交易的排序和打包。
	// 维护 L2 网络的最新状态，包括安全头和未确认的区块。
	n.l2Driver = driver.NewDriver(n.eventSys, n.eventDrain, &cfg.Driver, &cfg.Rollup, n.l2Source, n.l1Source,
		n.supervisor, n.beacon, n, n, n.log, n.metrics, cfg.ConfigPersistence, n.safeDB, &cfg.Sync, sequencerConductor, altDA)
	return nil
}

// initRPCServer RPC 服务器为 OpNode 提供了一个标准化的对外接口，允许其他程序、服务或用户与节点进行交互。
func (n *OpNode) initRPCServer(cfg *Config) error {
	// 创建新的 RPC 服务器
	server, err := newRPCServer(&cfg.RPC, &cfg.Rollup, n.l2Source.L2Client, n.l2Driver, n.safeDB, n.log, n.appVersion, n.metrics)
	if err != nil {
		return err
	}
	// 如果启用了 P2P 功能，为 RPC 服务器添加 P2P API
	if n.p2pEnabled() {
		server.EnableP2P(p2p.NewP2PAPIBackend(n.p2pNode, n.log, n.metrics))
	}
	// 如果配置中启用了管理员 API，为 RPC 服务器添加管理员 API
	if cfg.RPC.EnableAdmin {
		server.EnableAdminAPI(NewAdminAPI(n.l2Driver, n.metrics, n.log))
		n.log.Info("Admin RPC enabled")
	}
	// 启动 JSON-RPC 服务器
	n.log.Info("Starting JSON-RPC server")
	if err := server.Start(); err != nil {
		return fmt.Errorf("unable to start RPC server: %w", err)
	}
	// 将启动的服务器保存到 OpNode 实例中
	n.server = server
	return nil
}

func (n *OpNode) initMetricsServer(cfg *Config) error {
	if !cfg.Metrics.Enabled {
		n.log.Info("metrics disabled")
		return nil
	}
	n.log.Debug("starting metrics server", "addr", cfg.Metrics.ListenAddr, "port", cfg.Metrics.ListenPort)
	metricsSrv, err := n.metrics.StartServer(cfg.Metrics.ListenAddr, cfg.Metrics.ListenPort)
	if err != nil {
		return fmt.Errorf("failed to start metrics server: %w", err)
	}
	n.log.Info("started metrics server", "addr", metricsSrv.Addr())
	n.metricsSrv = metricsSrv
	return nil
}

func (n *OpNode) initPProf(cfg *Config) error {
	// 创建新的 pprof 服务实例
	n.pprofService = oppprof.New(
		// 是否启用 pprof 监听
		cfg.Pprof.ListenEnabled,
		// pprof 服务监听的地址
		cfg.Pprof.ListenAddr,
		// pprof 服务监听的端口
		cfg.Pprof.ListenPort,
		// 性能分析类型
		cfg.Pprof.ProfileType,
		// 性能分析文件保存的目录
		cfg.Pprof.ProfileDir,
		// 性能分析文件的文件名
		cfg.Pprof.ProfileFilename,
	)
	// 启动 pprof 服务
	if err := n.pprofService.Start(); err != nil {
		return fmt.Errorf("failed to start pprof service: %w", err)
	}

	return nil
}

func (n *OpNode) p2pEnabled() bool {
	return n.cfg.P2PEnabled()
}

func (n *OpNode) initP2P(cfg *Config) (err error) {
	// 检查 P2P 节点是否已经初始化
	if n.p2pNode != nil {
		panic("p2p node already initialized")
	}
	// 检查是否启用了 P2P 功能
	if n.p2pEnabled() {
		// TODO(protocol-quest#97): Use EL Sync instead of CL Alt sync for fetching missing blocks in the payload queue.
		// 创建新的 P2P 节点
		n.p2pNode, err = p2p.NewNodeP2P(n.resourcesCtx, &cfg.Rollup, n.log, cfg.P2P, n, n.l2Source, n.runCfg, n.metrics, false)
		if err != nil {
			return
		}
		// 如果启用了 Discv5 UDP 发现协议，启动发现进程
		if n.p2pNode.Dv5Udp() != nil {
			go n.p2pNode.DiscoveryProcess(n.resourcesCtx, n.log, &cfg.Rollup, cfg.P2P.TargetPeers())
		}
	}
	return nil
}

// initP2PSigner 初始化 P2P（点对点）网络的签名器。签名器用于对 P2P 网络中传输的消息进行签名，以确保消息的真实性和完整性。
func (n *OpNode) initP2PSigner(ctx context.Context, cfg *Config) (err error) {
	// the p2p signer setup is optional
	// 检查配置中是否设置了 P2P 签名器
	// P2P 签名器的设置是可选的
	if cfg.P2PSigner == nil {
		return
	}
	// p2pSigner may still be nil, the signer setup may not create any signer, the signer is optional
	// 设置 P2P 签名器
	// 即使在设置过程中，p2pSigner 可能仍然为 nil，因为签名器是可选的
	n.p2pSigner, err = cfg.P2PSigner.SetupSigner(ctx)
	return
}

func (n *OpNode) Start(ctx context.Context) error {
	n.log.Info("Starting execution engine driver")
	// start driving engine: sync blocks by deriving them from L1 and driving them into the engine
	if err := n.l2Driver.Start(); err != nil {
		n.log.Error("Could not start a rollup node", "err", err)
		return err
	}
	log.Info("Rollup node started")
	return nil
}

// onEvent handles broadcast events.
// The OpNode itself is a deriver to catch system-critical events.
// Other event-handling should be encapsulated into standalone derivers.
func (n *OpNode) onEvent(ev event.Event) bool {
	switch x := ev.(type) {
	case rollup.CriticalErrorEvent:
		n.log.Error("Critical error", "err", x.Err)
		n.cancel(fmt.Errorf("critical error: %w", x.Err))
		return true
	default:
		return false
	}
}

func (n *OpNode) OnNewL1Head(ctx context.Context, sig eth.L1BlockRef) {
	// 调用 tracer 的 OnNewL1Head 方法，记录新的 L1 头部信息。
	n.tracer.OnNewL1Head(ctx, sig)

	if n.l2Driver == nil {
		return
	}
	// Pass on the event to the L2 Engine
	ctx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	// 通知 L2 驱动有关 L1 头部变化的信息。
	if err := n.l2Driver.OnL1Head(ctx, sig); err != nil {
		n.log.Warn("failed to notify engine driver of L1 head change", "err", err)
	}
}

func (n *OpNode) OnNewL1Safe(ctx context.Context, sig eth.L1BlockRef) {
	if n.l2Driver == nil {
		return
	}
	// Pass on the event to the L2 Engine
	ctx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	// 通知 L2 驱动有关 L1 安全区块变化的信息。
	if err := n.l2Driver.OnL1Safe(ctx, sig); err != nil {
		n.log.Warn("failed to notify engine driver of L1 safe block change", "err", err)
	}
}

func (n *OpNode) OnNewL1Finalized(ctx context.Context, sig eth.L1BlockRef) {
	if n.l2Driver == nil {
		return
	}
	// Pass on the event to the L2 Engine
	ctx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	// 通知 L2 驱动有关 L1 最终确认区块变化的信息。
	if err := n.l2Driver.OnL1Finalized(ctx, sig); err != nil {
		n.log.Warn("failed to notify engine driver of L1 finalized block change", "err", err)
	}
}

func (n *OpNode) PublishL2Payload(ctx context.Context, envelope *eth.ExecutionPayloadEnvelope) error {
	n.tracer.OnPublishL2Payload(ctx, envelope)

	// publish to p2p, if we are running p2p at all
	if n.p2pEnabled() {
		payload := envelope.ExecutionPayload
		if n.p2pSigner == nil {
			return fmt.Errorf("node has no p2p signer, payload %s cannot be published", payload.ID())
		}
		n.log.Info("Publishing signed execution payload on p2p", "id", payload.ID())
		return n.p2pNode.GossipOut().PublishL2Payload(ctx, envelope, n.p2pSigner)
	}
	// if p2p is not enabled then we just don't publish the payload
	return nil
}

func (n *OpNode) OnUnsafeL2Payload(ctx context.Context, from peer.ID, envelope *eth.ExecutionPayloadEnvelope) error {
	// ignore if it's from ourselves
	if n.p2pEnabled() && from == n.p2pNode.Host().ID() {
		return nil
	}

	n.tracer.OnUnsafeL2Payload(ctx, from, envelope)

	n.log.Info("Received signed execution payload from p2p", "id", envelope.ExecutionPayload.ID(), "peer", from,
		"txs", len(envelope.ExecutionPayload.Transactions))

	// Pass on the event to the L2 Engine
	ctx, cancel := context.WithTimeout(ctx, time.Second*30)
	defer cancel()

	if err := n.l2Driver.OnUnsafeL2Payload(ctx, envelope); err != nil {
		n.log.Warn("failed to notify engine driver of new L2 payload", "err", err, "id", envelope.ExecutionPayload.ID())
	}

	return nil
}

func (n *OpNode) RequestL2Range(ctx context.Context, start, end eth.L2BlockRef) error {
	// 检查 P2P 是否启用且替代同步功能是否开启
	if n.p2pEnabled() && n.p2pNode.AltSyncEnabled() {
		// 检查起始区块的时间戳是否过旧（超过12小时）
		if unixTimeStale(start.Time, 12*time.Hour) {
			// 如果过旧，记录日志并忽略请求
			n.log.Debug(
				"ignoring request to sync L2 range, timestamp is too old for p2p",
				"start", start,
				"end", end,
				"start_time", start.Time)
			return nil
		}
		// 如果时间戳合适，通过 P2P 网络请求同步
		return n.p2pNode.RequestL2Range(ctx, start, end)
	}
	n.log.Debug("ignoring request to sync L2 range, no sync method available", "start", start, "end", end)
	return nil
}

// unixTimeStale returns true if the unix timestamp is before the current time minus the supplied duration.
func unixTimeStale(timestamp uint64, duration time.Duration) bool {
	return time.Unix(int64(timestamp), 0).Before(time.Now().Add(-1 * duration))
}

func (n *OpNode) P2P() p2p.Node {
	return n.p2pNode
}

func (n *OpNode) RuntimeConfig() ReadonlyRuntimeConfig {
	return n.runCfg
}

// Stop stops the node and closes all resources.
// If the provided ctx is expired, the node will accelerate the stop where possible, but still fully close.
func (n *OpNode) Stop(ctx context.Context) error {
	if n.closed.Load() {
		return ErrAlreadyClosed
	}

	var result *multierror.Error

	if n.server != nil {
		if err := n.server.Stop(ctx); err != nil {
			result = multierror.Append(result, fmt.Errorf("failed to close RPC server: %w", err))
		}
	}

	// Stop sequencer and report last hash. l2Driver can be nil if we're cleaning up a failed init.
	if n.l2Driver != nil {
		latestHead, err := n.l2Driver.StopSequencer(ctx)
		switch {
		case errors.Is(err, sequencing.ErrSequencerNotEnabled):
		case errors.Is(err, driver.ErrSequencerAlreadyStopped):
			n.log.Info("stopping node: sequencer already stopped", "latestHead", latestHead)
		case err == nil:
			n.log.Info("stopped sequencer", "latestHead", latestHead)
		default:
			result = multierror.Append(result, fmt.Errorf("error stopping sequencer: %w", err))
		}
	}
	if n.p2pNode != nil {
		if err := n.p2pNode.Close(); err != nil {
			result = multierror.Append(result, fmt.Errorf("failed to close p2p node: %w", err))
		}
		// Prevent further use of p2p.
		n.p2pNode = nil
	}
	if n.p2pSigner != nil {
		if err := n.p2pSigner.Close(); err != nil {
			result = multierror.Append(result, fmt.Errorf("failed to close p2p signer: %w", err))
		}
	}

	if n.resourcesClose != nil {
		n.resourcesClose()
	}

	// stop L1 heads feed
	if n.l1HeadsSub != nil {
		n.l1HeadsSub.Unsubscribe()
	}
	// stop polling for L1 safe-head changes
	if n.l1SafeSub != nil {
		n.l1SafeSub.Unsubscribe()
	}
	// stop polling for L1 finalized-head changes
	if n.l1FinalizedSub != nil {
		n.l1FinalizedSub.Unsubscribe()
	}

	// close L2 driver
	if n.l2Driver != nil {
		if err := n.l2Driver.Close(); err != nil {
			result = multierror.Append(result, fmt.Errorf("failed to close L2 engine driver cleanly: %w", err))
		}
	}

	if n.eventSys != nil {
		n.eventSys.Stop()
	}

	if n.safeDB != nil {
		if err := n.safeDB.Close(); err != nil {
			result = multierror.Append(result, fmt.Errorf("failed to close safe head db: %w", err))
		}
	}

	// Wait for the runtime config loader to be done using the data sources before closing them
	if n.runtimeConfigReloaderDone != nil {
		<-n.runtimeConfigReloaderDone
	}

	// close L2 engine RPC client
	if n.l2Source != nil {
		n.l2Source.Close()
	}

	// close the supervisor RPC client
	if n.supervisor != nil {
		n.supervisor.Close()
	}

	// close L1 data source
	if n.l1Source != nil {
		n.l1Source.Close()
	}

	if result == nil { // mark as closed if we successfully fully closed
		n.closed.Store(true)
	}

	if n.halted.Load() {
		// if we had a halt upon initialization, idle for a while, with open metrics, to prevent a rapid restart-loop
		tim := time.NewTimer(time.Minute * 5)
		n.log.Warn("halted, idling to avoid immediate shutdown repeats")
		defer tim.Stop()
		select {
		case <-tim.C:
		case <-ctx.Done():
		}
	}

	// Close metrics and pprof only after we are done idling
	if n.pprofService != nil {
		if err := n.pprofService.Stop(ctx); err != nil {
			result = multierror.Append(result, fmt.Errorf("failed to close pprof server: %w", err))
		}
	}
	if n.metricsSrv != nil {
		if err := n.metricsSrv.Stop(ctx); err != nil {
			result = multierror.Append(result, fmt.Errorf("failed to close metrics server: %w", err))
		}
	}

	return result.ErrorOrNil()
}

func (n *OpNode) Stopped() bool {
	return n.closed.Load()
}

func (n *OpNode) HTTPEndpoint() string {
	if n.server == nil {
		return ""
	}
	return fmt.Sprintf("http://%s", n.server.Addr().String())
}
