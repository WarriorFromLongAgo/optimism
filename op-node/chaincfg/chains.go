package chaincfg

import (
	"fmt"
	"strings"

	"github.com/ethereum-optimism/superchain-registry/superchain"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
)

var Mainnet, Sepolia *rollup.Config

// init 定义和初始化主网(Mainnet)和 Sepolia 测试网的 rollup 配置。
func init() {
	mustCfg := func(name string) *rollup.Config {
		cfg, err := GetRollupConfig(name)
		if err != nil {
			panic(fmt.Errorf("failed to load rollup config %q: %w", name, err))
		}
		return cfg
	}
	Mainnet = mustCfg("op-mainnet")
	Sepolia = mustCfg("op-sepolia")
}

// L2ChainIDToNetworkDisplayName 创建一个从 L2 链 ID 到网络显示名称的映射。
var L2ChainIDToNetworkDisplayName = func() map[string]string {
	out := make(map[string]string)
	for _, netCfg := range superchain.OPChains {
		out[fmt.Sprintf("%d", netCfg.ChainID)] = netCfg.Name
	}
	return out
}()

// AvailableNetworks returns the selection of network configurations that is available by default.
// 返回默认可用的网络配置选择。
func AvailableNetworks() []string {
	var networks []string
	for _, cfg := range superchain.OPChains {
		networks = append(networks, cfg.Chain+"-"+cfg.Superchain)
	}
	return networks
}

// 处理网络名称的向后兼容性,将旧的名称映射到新的格式
func handleLegacyName(name string) string {
	switch name {
	case "mainnet":
		return "op-mainnet"
	case "sepolia":
		return "op-sepolia"
	default:
		return name
	}
}

// ChainByName returns a chain, from known available configurations, by name.
// ChainByName returns nil when the chain name is unknown.
// ChainByName 根据名称从已知可用配置中返回一个链。
// 当链名称未知时，ChainByName 返回 nil。
func ChainByName(name string) *superchain.ChainConfig {
	// Handle legacy name aliases
	name = handleLegacyName(name)
	for _, chainCfg := range superchain.OPChains {
		if strings.EqualFold(chainCfg.Chain+"-"+chainCfg.Superchain, name) {
			return chainCfg
		}
	}
	return nil
}

// GetRollupConfig 加载特定网络的 rollup 配置
func GetRollupConfig(name string) (*rollup.Config, error) {
	chainCfg := ChainByName(name)
	if chainCfg == nil {
		return nil, fmt.Errorf("invalid network: %q", name)
	}
	rollupCfg, err := rollup.LoadOPStackRollupConfig(chainCfg.ChainID)
	if err != nil {
		return nil, fmt.Errorf("failed to load rollup config: %w", err)
	}
	return rollupCfg, nil
}
