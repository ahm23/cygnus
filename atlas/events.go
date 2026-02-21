package atlas

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/cometbft/cometbft/rpc/client/http"
	wstypes "github.com/cometbft/cometbft/rpc/core/types"
	ctypes "github.com/cometbft/cometbft/types"
	"go.uber.org/zap"

	"cygnus/config"
)

const (
	// Tx subscription: only specific file/subscription actions
	queryTxActions = `tm.event='Tx' AND message.action='delete_file'`
	// `(message.action='delete_file' ` +
	// `OR message.action='create_file' ` +
	// `OR message.action='create_subscription')`

	// Block subscription: receive every new block (to catch EndBlock events)
	queryNewBlock = `tm.event='NewBlock'`

	// Cosmos SDK / module event keys
	attrFileID       = "file_id"
	eventTypeMessage = "message"

	// Known actions from txs
	actionDeleteFile = "delete_file"

	// Example endblocker event types / keys
	// Common patterns: "proof.start_proof_round", "start_proof_window", "round.started", etc.
	endblockProofRoundKey  = "challenge_round_start.round" // or "proof.round_started", "round.id", etc.
	endblockProofWindowKey = "challenge_window_start.window"
)

// ChainEventReceiver defines callbacks for relevant events
type ChainEventReceiver interface {
	// Tx events
	OnFileDeleted(ctx context.Context, fileID string) error

	// EndBlock events
	OnStartProofRound(ctx context.Context, height int64, roundOrData string) error
	OnStartProofWindow(ctx context.Context, height int64, windowOrData string) error
}

// EventListener subscribes to tx + block events and dispatches them.
type EventListener struct {
	cfg      *config.Config
	logger   *zap.Logger
	receiver ChainEventReceiver

	client *http.HTTP
	done   chan struct{}
}

// NewEventListener ...
func NewEventListener(cfg *config.Config, logger *zap.Logger, receiver ChainEventReceiver) (*EventListener, error) {
	rpcAddr := strings.TrimSuffix(cfg.ChainCfg.RPCAddr, "/")
	if !strings.HasPrefix(rpcAddr, "http://") && !strings.HasPrefix(rpcAddr, "https://") {
		rpcAddr = "http://" + rpcAddr
	}
	wsPath := "/websocket"

	client, err := http.New(rpcAddr, wsPath)
	if err != nil {
		return nil, fmt.Errorf("atlas events: create rpc client: %w", err)
	}

	return &EventListener{
		cfg:      cfg,
		logger:   logger,
		receiver: receiver,
		client:   client,
		done:     make(chan struct{}),
	}, nil
}

// Start begins both subscriptions and processes events.
func (el *EventListener) Start(ctx context.Context) error {
	el.logger.Info("[EventListener] Starting event listener...")
	if err := el.client.Start(); err != nil {
		return fmt.Errorf("atlas events: start rpc client: %w", err)
	}
	defer el.client.Stop()

	// ── Tx subscription ────────────────────────────────────────────────────────
	txCh, err := el.client.Subscribe(ctx, "cygnus-tx-actions", queryTxActions, 128)
	if err != nil {
		return fmt.Errorf("subscribe tx actions: %w", err)
	}
	defer func() { _ = el.client.Unsubscribe(ctx, "", "cygnus-tx-actions") }()

	// ── NewBlock subscription ──────────────────────────────────────────────────
	blockCh, err := el.client.Subscribe(ctx, "cygnus-new-block", queryNewBlock, 64)
	if err != nil {
		return fmt.Errorf("subscribe new block: %w", err)
	}
	defer func() { _ = el.client.Unsubscribe(ctx, "", "cygnus-new-block") }()

	el.logger.Info("Subscribed to tx actions + new blocks",
		zap.String("rpc", el.cfg.ChainCfg.RPCAddr),
		zap.String("tx_query", queryTxActions),
		zap.String("block_query", queryNewBlock))

	// Fan-in both channels
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-el.done:
			return nil

		case res, ok := <-txCh:
			if !ok {
				return fmt.Errorf("tx subscription channel closed")
			}
			el.handleTxEvent(ctx, res)

		case res, ok := <-blockCh:
			if !ok {
				return fmt.Errorf("block subscription channel closed")
			}
			el.handleBlockEvent(ctx, res)
		}
	}
}

// Stop signals shutdown.
func (el *EventListener) Stop() {
	select {
	case <-el.done:
	default:
		close(el.done)
	}
}

// ── Tx event handling ────────────────────────────────────────────────────────

func (el *EventListener) handleTxEvent(ctx context.Context, result wstypes.ResultEvent) {
	events := result.Events
	if events == nil {
		return
	}

	fileID := getFileID(events)
	if fileID == "" {
		el.logger.Warn("Tx event without file_id", zap.Any("events", events))
		return
	}

	actionVals := events[eventTypeMessage+".action"]
	if len(actionVals) == 0 {
		el.logger.Warn("Tx event without message.action")
		return
	}
	action := actionVals[0]

	switch action {
	case actionDeleteFile:
		el.dispatchOrLog(ctx, "delete_file", fileID, el.receiver.OnFileDeleted(ctx, fileID))
	default:
		el.logger.Warn("Unexpected tx action in filtered sub", zap.String("action", action))
	}
}

// ── Block (EndBlock) event handling ─────────────────────────────────────────
func (el *EventListener) handleBlockEvent(ctx context.Context, result wstypes.ResultEvent) {
	// el.logger.Info("[EventListener] New block event!")
	// el.logger.Info(fmt.Sprint(result.Events))

	block := result.Data.(ctypes.EventDataNewBlock).Block
	events := result.Events
	if events == nil {
		return
	}

	// 1. Extract block height from the standard indexed key (safest way)
	height := block.Height

	// extra: confirm it's really a NewBlock
	tmEventVals, hasTm := events["tm.event"]
	if !hasTm || len(tmEventVals) == 0 || tmEventVals[0] != "NewBlock" {
		el.logger.Warn("Unexpected tm.event in block subscription")
		return
	}

	// 2. Now look for your custom EndBlock events
	if _, hasRound := events[endblockProofRoundKey]; hasRound {
		el.logger.Info(fmt.Sprint(events[endblockProofRoundKey]))
		round := getFirstOrEmpty(events, endblockProofRoundKey+".round")
		el.dispatchOrLog(ctx, "challenge_round_start",
			fmt.Sprintf("height=%d round=%s", height, round),
			el.receiver.OnStartProofRound(ctx, height, round))
	}

	if _, hasWindow := events[endblockProofWindowKey]; hasWindow {
		window := getFirstOrEmpty(events, endblockProofWindowKey+".window")
		el.dispatchOrLog(ctx, "challenge_window_start",
			fmt.Sprintf("height=%d window=%s", height, window),
			el.receiver.OnStartProofWindow(ctx, height, window))
	}
}

// Helpers ─────────────────────────────────────────────────────────────────────
func getFileID(events map[string][]string) string {
	vals := events[attrFileID]
	if len(vals) > 0 {
		return vals[0]
	}
	vals = events["message.file_id"]
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func getFirstOrEmpty(events map[string][]string, key string) string {
	vals, ok := events[key]
	if ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func (el *EventListener) dispatchOrLog(ctx context.Context, kind, id string, err error) {
	if err != nil {
		el.logger.Error("Handle event failed",
			zap.String("kind", kind),
			zap.String("id", id),
			zap.Error(err))
	} else {
		el.logger.Info("Processed event",
			zap.String("kind", kind),
			zap.String("id", id))
	}
}

// RPCWebSocketURL returns the WebSocket URL for the configured RPC (for reference).
func RPCWebSocketURL(rpcAddr string) string {
	rpcAddr = strings.TrimSuffix(rpcAddr, "/")
	if rpcAddr == "" {
		return ""
	}
	if !strings.HasPrefix(rpcAddr, "http://") && !strings.HasPrefix(rpcAddr, "https://") {
		rpcAddr = "http://" + rpcAddr
	}
	u, err := url.Parse(rpcAddr)
	if err != nil {
		return rpcAddr + "/websocket"
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/websocket"
	return u.String()
}
