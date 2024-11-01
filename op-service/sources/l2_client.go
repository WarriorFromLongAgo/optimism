package sources

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/client"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/predeploys"
	"github.com/ethereum-optimism/optimism/op-service/sources/caching"
)

type L2ClientConfig struct {
	EthClientConfig

	L2BlockRefsCacheSize int
	L1ConfigsCacheSize   int

	RollupCfg *rollup.Config
}

func L2ClientDefaultConfig(config *rollup.Config, trustRPC bool) *L2ClientConfig {
	// Cache 3/2 worth of sequencing window of payloads, block references, receipts and txs
	span := int(config.SeqWindowSize) * 3 / 2
	// Estimate number of L2 blocks in this span of L1 blocks
	// (there's always one L2 block per L1 block, L1 is thus the minimum, even if block time is very high)
	if config.BlockTime < 12 && config.BlockTime > 0 {
		span *= 12
		span /= int(config.BlockTime)
	}
	fullSpan := span
	if span > 1000 { // sanity cap. If a large sequencing window is configured, do not make the cache too large
		span = 1000
	}
	return &L2ClientConfig{
		EthClientConfig: EthClientConfig{
			// receipts and transactions are cached per block
			ReceiptsCacheSize:     span,
			TransactionsCacheSize: span,
			HeadersCacheSize:      span,
			PayloadsCacheSize:     span,
			MaxRequestsPerBatch:   20, // TODO: tune batch param
			MaxConcurrentRequests: 10,
			TrustRPC:              trustRPC,
			MustBePostMerge:       true,
			RPCProviderKind:       RPCKindStandard,
			MethodResetDuration:   time.Minute,
		},
		// Not bounded by span, to cover find-sync-start range fully for speedy recovery after errors.
		L2BlockRefsCacheSize: fullSpan,
		L1ConfigsCacheSize:   span,
		RollupCfg:            config,
	}
}

// L2Client extends EthClient with functions to fetch and cache eth.L2BlockRef values.
type L2Client struct {
	*EthClient
	rollupCfg *rollup.Config

	// cache L2BlockRef by hash
	// common.Hash -> eth.L2BlockRef
	l2BlockRefsCache *caching.LRUCache[common.Hash, eth.L2BlockRef]

	// cache SystemConfig by L2 hash
	// common.Hash -> eth.SystemConfig
	systemConfigsCache *caching.LRUCache[common.Hash, eth.SystemConfig]
}

// NewL2Client constructs a new L2Client instance. The L2Client is a thin wrapper around the EthClient with added functions
// for fetching and caching eth.L2BlockRef values. This includes fetching an L2BlockRef by block number, label, or hash.
// See: [L2BlockRefByLabel], [L2BlockRefByNumber], [L2BlockRefByHash]
func NewL2Client(client client.RPC, log log.Logger, metrics caching.Metrics, config *L2ClientConfig) (*L2Client, error) {
	ethClient, err := NewEthClient(client, log, metrics, &config.EthClientConfig)
	if err != nil {
		return nil, err
	}

	return &L2Client{
		EthClient:          ethClient,
		rollupCfg:          config.RollupCfg,
		l2BlockRefsCache:   caching.NewLRUCache[common.Hash, eth.L2BlockRef](metrics, "blockrefs", config.L2BlockRefsCacheSize),
		systemConfigsCache: caching.NewLRUCache[common.Hash, eth.SystemConfig](metrics, "systemconfigs", config.L1ConfigsCacheSize),
	}, nil
}

func (s *L2Client) RollupConfig() *rollup.Config {
	return s.rollupCfg
}

// L2BlockRefByLabel returns the [eth.L2BlockRef] for the given block label.
// L2BlockRefByLabel 返回给定块标签的 [eth.L2BlockRef]。
func (s *L2Client) L2BlockRefByLabel(ctx context.Context, label eth.BlockLabel) (eth.L2BlockRef, error) {
	envelope, err := s.PayloadByLabel(ctx, label)
	if err != nil {
		// Both geth and erigon like to serve non-standard errors for the safe and finalized heads, correct that.
		// This happens when the chain just started and nothing is marked as safe/finalized yet.
		if strings.Contains(err.Error(), "block not found") || strings.Contains(err.Error(), "Unknown block") || strings.Contains(err.Error(), "unknown block") || strings.Contains(err.Error(), "header not found") {
			err = ethereum.NotFound
		}
		// w%: wrap to preserve ethereum.NotFound case
		return eth.L2BlockRef{}, fmt.Errorf("failed to determine L2BlockRef of %s, could not get payload: %w", label, err)
	}
	ref, err := derive.PayloadToBlockRef(s.rollupCfg, envelope.ExecutionPayload)
	if err != nil {
		return eth.L2BlockRef{}, err
	}
	s.l2BlockRefsCache.Add(ref.Hash, ref)
	return ref, nil
}

// L2BlockRefByNumber returns the [eth.L2BlockRef] for the given block number.
func (s *L2Client) L2BlockRefByNumber(ctx context.Context, num uint64) (eth.L2BlockRef, error) {
	envelope, err := s.PayloadByNumber(ctx, num)
	if err != nil {
		// w%: wrap to preserve ethereum.NotFound case
		return eth.L2BlockRef{}, fmt.Errorf("failed to determine L2BlockRef of height %v, could not get payload: %w", num, err)
	}
	ref, err := derive.PayloadToBlockRef(s.rollupCfg, envelope.ExecutionPayload)
	if err != nil {
		return eth.L2BlockRef{}, err
	}
	s.l2BlockRefsCache.Add(ref.Hash, ref)
	return ref, nil
}

// L2BlockRefByHash returns the [eth.L2BlockRef] for the given block hash.
// The returned BlockRef may not be in the canonical chain.
func (s *L2Client) L2BlockRefByHash(ctx context.Context, hash common.Hash) (eth.L2BlockRef, error) {
	if ref, ok := s.l2BlockRefsCache.Get(hash); ok {
		return ref, nil
	}

	envelope, err := s.PayloadByHash(ctx, hash)
	if err != nil {
		// w%: wrap to preserve ethereum.NotFound case
		return eth.L2BlockRef{}, fmt.Errorf("failed to determine block-hash of hash %v, could not get payload: %w", hash, err)
	}
	ref, err := derive.PayloadToBlockRef(s.rollupCfg, envelope.ExecutionPayload)
	if err != nil {
		return eth.L2BlockRef{}, err
	}
	s.l2BlockRefsCache.Add(ref.Hash, ref)
	return ref, nil
}

// SystemConfigByL2Hash returns the [eth.SystemConfig] (matching the config updates up to and including the L1 origin) for the given L2 block hash.
// The returned [eth.SystemConfig] may not be in the canonical chain when the hash is not canonical.
// SystemConfigByL2Hash 返回给定 L2 块哈希的 [eth.SystemConfig]（匹配配置更新直至 L1 原点）。
// 当哈希不是规范的时，返回的 [eth.SystemConfig] 可能不在规范链中。
// 方法的主要功能是根据给定的 L2 区块哈希获取对应的系统配置（SystemConfig）。这个方法首先尝试从缓存中获取配置，如果缓存中没有，则从区块数据中提取配置信息。
// L2 区块包含 L1 信息：每个 L2 区块都包含一个特殊的交易，通常是第一个交易，它包含了相关的 L1 信息。这个交易被称为 "L1 info deposit transaction"。
// L1 信息中包含 SystemConfig：这个 L1 信息交易中包含了当前的 SystemConfig 数据。
// SystemConfig 它代表了在每个 L2 区块中传递的 rollup 系统配置，这个配置可以通过 L1 系统配置事件进行更改。
// BatcherAddr: 用于识别批处理发送者地址，用于批处理收件箱数据交易过滤。
// Overhead: 标识 L1 费用开销。在 Ecotone 升级之前，这个值会直接传递给引擎；升级后，这个值始终为零，不会传递给引擎。
// calar: 标识 L1 费用标量。在 Ecotone 升级之前，这个值会直接传递给引擎；升级后，这个值编码了多个标量数据。
// GasLimit: 标识 L2 区块的燃气限制。
func (s *L2Client) SystemConfigByL2Hash(ctx context.Context, hash common.Hash) (eth.SystemConfig, error) {
	// 尝试从缓存中获取系统配置
	if ref, ok := s.systemConfigsCache.Get(hash); ok {
		return ref, nil
	}
	// 如果缓存中没有，则通过区块哈希获取区块数据
	envelope, err := s.PayloadByHash(ctx, hash)
	if err != nil {
		// w%: wrap to preserve ethereum.NotFound case
		// 如果获取失败，返回错误，保留 ethereum.NotFound 错误类型
		return eth.SystemConfig{}, fmt.Errorf("failed to determine block-hash of hash %v, could not get payload: %w", hash, err)
	}
	// 从区块数据中提取系统配置
	cfg, err := derive.PayloadToSystemConfig(s.rollupCfg, envelope.ExecutionPayload)
	if err != nil {
		return eth.SystemConfig{}, err
	}
	// 将提取的系统配置添加到缓存中
	s.systemConfigsCache.Add(hash, cfg)
	return cfg, nil
}

func (s *L2Client) OutputV0AtBlock(ctx context.Context, blockHash common.Hash) (*eth.OutputV0, error) {
	head, err := s.InfoByHash(ctx, blockHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get L2 block by hash: %w", err)
	}
	if head == nil {
		return nil, ethereum.NotFound
	}

	proof, err := s.GetProof(ctx, predeploys.L2ToL1MessagePasserAddr, []common.Hash{}, blockHash.String())
	if err != nil {
		return nil, fmt.Errorf("failed to get contract proof at block %s: %w", blockHash, err)
	}
	if proof == nil {
		return nil, fmt.Errorf("proof %w", ethereum.NotFound)
	}
	// make sure that the proof (including storage hash) that we retrieved is correct by verifying it against the state-root
	if err := proof.Verify(head.Root()); err != nil {
		return nil, fmt.Errorf("invalid withdrawal root hash, state root was %s: %w", head.Root(), err)
	}
	stateRoot := head.Root()
	return &eth.OutputV0{
		StateRoot:                eth.Bytes32(stateRoot),
		MessagePasserStorageRoot: eth.Bytes32(proof.StorageHash),
		BlockHash:                blockHash,
	}, nil
}
