package atlas

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cometbft/cometbft/rpc/client/http"
	wstypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/rs/zerolog/log"

	"cygnus/config"
)

const (
	// Tx subscription: only specific file/subscription actions
	queryTxActions = `tm.event='Tx' AND message.action='delete_file'`
	// `(message.action='delete_file' ` +
	// `OR message.action='create_file')`

	// Block event subscription: governance proposals that pass
	queryGovProposalPassed = `active_proposal.proposal_result='proposal_passed'`

	// CosmosSDK module event keys
	attrFID          = "fid"
	eventTypeMessage = "message"

	// known actions from txs
	actionDeleteFile = "delete_file"
)

// ChainEventReceiver defines callbacks for relevant events
type ChainEventReceiver interface {
	// Tx events
	OnFileDeleted(ctx context.Context, fileID string) error

	// Block events
	OnProposalPassed(ctx context.Context, proposalID uint64) error
}

// EventListener subscribes to tx events and dispatches them.
type EventListener struct {
	cfg      *config.Config
	receiver ChainEventReceiver

	client *http.HTTP
	done   chan struct{}
}

func NewEventListener(cfg *config.Config, receiver ChainEventReceiver) (*EventListener, error) {
	rpcAddr := strings.TrimSuffix(cfg.ChainCfg.RPCAddr, "/")
	if !strings.HasPrefix(rpcAddr, "http://") && !strings.HasPrefix(rpcAddr, "https://") {
		rpcAddr = "http://" + rpcAddr
	}

	// resolve the WebSocket endpoint — explicit ws_addr wins, otherwise
	// derive from the HTTP RPC address (scheme swap + /websocket).
	wsEndpoint := strings.TrimSuffix(cfg.ChainCfg.WSAddr, "/")
	if wsEndpoint == "" {
		wsEndpoint = "/websocket"
	}

	client, err := http.New(rpcAddr, wsEndpoint)
	if err != nil {
		return nil, fmt.Errorf("atlas events: create rpc client: %w", err)
	}

	return &EventListener{
		cfg:      cfg,
		receiver: receiver,
		client:   client,
		done:     make(chan struct{}),
	}, nil
}

// Start begins subscriptions and dispatches events.
func (el *EventListener) Start(ctx context.Context) error {
	if err := el.client.Start(); err != nil {
		return fmt.Errorf("failed to start rpc client: %w", err)
	}
	defer el.client.Stop()
	log.Info().Msg("Event listener started!")

	// tx subscription
	txCh, err := el.client.Subscribe(ctx, "cygnus-tx-actions", queryTxActions, 128)
	if err != nil {
		return fmt.Errorf("failed to subscribe to tx actions: %w", err)
	}
	defer el.client.Unsubscribe(ctx, "", "cygnus-tx-actions")
	log.Info().Str("tx_query", queryTxActions).Msg("Subscribed to tx actions")

	// governance proposal passed subscription
	govCh, err := el.client.Subscribe(ctx, "cygnus-gov-passed", queryGovProposalPassed, 128)
	if err != nil {
		return fmt.Errorf("failed to subscribe to governance proposals: %w", err)
	}
	defer el.client.Unsubscribe(ctx, "", "cygnus-gov-passed")
	log.Info().Str("gov_query", queryGovProposalPassed).Msg("Subscribed to governance proposals")

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

		case res, ok := <-govCh:
			if !ok {
				return fmt.Errorf("gov subscription channel closed")
			}
			el.handleGovEvent(ctx, res)
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

// handleTxEvent handles certain tx events appropriately.
func (el *EventListener) handleTxEvent(ctx context.Context, result wstypes.ResultEvent) {
	events := result.Events
	if events == nil {
		return
	}

	fid := getFID(events)
	if fid == "" {
		log.Warn().Any("events", events).Msg("Tx event without fid")
		return
	}

	actionVals := events[eventTypeMessage+".action"]
	if len(actionVals) == 0 {
		log.Warn().Msg("Tx event without message.action")
		return
	}
	action := actionVals[0]

	switch action {
	case actionDeleteFile:
		el.receiver.OnFileDeleted(ctx, fid)
	default:
		log.Warn().Str("action", action).Msg("Unexpected tx action in filtered subscription")
	}
}

// handleGovEvent handles governance proposal events.
func (el *EventListener) handleGovEvent(ctx context.Context, result wstypes.ResultEvent) {
	events := result.Events
	if events == nil {
		return
	}

	vals := events["active_proposal.proposal_result"]
	if len(vals) == 0 || vals[0] != "proposal_passed" {
		return
	}

	idVals := events["active_proposal.proposal_id"]
	if len(idVals) == 0 {
		log.Warn().Msg("Gov proposal passed event without proposal_id")
		return
	}

	proposalID, err := strconv.ParseUint(idVals[0], 10, 64)
	if err != nil {
		log.Warn().Err(err).Str("raw_id", idVals[0]).Msg("Gov proposal passed event with invalid proposal_id")
		return
	}

	log.Info().Uint64("proposal_id", proposalID).Msg("Governance proposal passed")
	if err := el.receiver.OnProposalPassed(ctx, proposalID); err != nil {
		log.Error().Err(err).Uint64("proposal_id", proposalID).Msg("OnProposalPassed handler failed")
	}
}

func getFID(events map[string][]string) string {
	vals := events[actionDeleteFile+"."+attrFID]
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
