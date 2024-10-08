package node

import (
	"context"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// Tracer configures the OpNode to share events
// 这个 Tracer 接口定义了一个用于追踪和记录 Optimism 节点关键事件的机制。
// 这是一个接口，定义了追踪器的基本行为。
type Tracer interface {
	// OnNewL1Head 监控 L1 头部更新：通过 OnNewL1Head 方法追踪新的 L1 区块头。
	OnNewL1Head(ctx context.Context, sig eth.L1BlockRef)
	// OnUnsafeL2Payload 追踪不安全的 L2 payload：OnUnsafeL2Payload 方法用于记录从其他节点接收到的未经验证的 L2 执行 payload。
	OnUnsafeL2Payload(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope)
	// OnPublishL2Payload 记录 L2 payload 的发布：OnPublishL2Payload 方法用于追踪节点发布新的 L2 执行 payload 的事件。
	OnPublishL2Payload(ctx context.Context, payload *eth.ExecutionPayloadEnvelope)
}

// 这是 Tracer 接口的一个简单实现。它实现了 Tracer 接口的所有方法，但所有方法都是空操作（no-op）。
// 这种实现通常用作默认的或空的追踪器，当不需要实际追踪功能时使用。
type noOpTracer struct{}

func (n noOpTracer) OnNewL1Head(ctx context.Context, sig eth.L1BlockRef) {}

func (n noOpTracer) OnUnsafeL2Payload(ctx context.Context, from peer.ID, payload *eth.ExecutionPayloadEnvelope) {
}

func (n noOpTracer) OnPublishL2Payload(ctx context.Context, payload *eth.ExecutionPayloadEnvelope) {}

var _ Tracer = (*noOpTracer)(nil)
