package atlas

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdktx "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	"go.uber.org/zap"

	"github.com/cosmos/cosmos-sdk/crypto/keyring"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"

	// Import from your local blockchain
	"atlas/app"

	"cygnus/config"
	"cygnus/types"
)

type AtlasWallet struct {
	mu     sync.RWMutex
	logger *zap.Logger

	kr           keyring.Keyring
	clientCtx    *client.Context
	queryClients *types.QueryClients
	txClient     sdktx.ServiceClient

	keyName       string
	address       sdk.Address
	accountNumber uint64
	sequence      uint64

	gasPrices     string
	gasAdjustment float64

	txQueue         chan *walletTxRequest
	highPriorityTxQ chan *walletTxRequest
	txStopChan      chan struct{}
	txWG            sync.WaitGroup
	txStopOnce      sync.Once
	txQueueMu       sync.RWMutex
	txStopped       bool
}

type TxPriority int

const (
	TxPriorityNormal TxPriority = iota
	TxPriorityHigh
)

type walletTxRequest struct {
	retries int
	wait    bool
	msgs    []sdk.Msg
	result  chan walletTxResult
}

type walletTxResult struct {
	resp *sdk.TxResponse
	err  error
}

const (
	walletTxQueueSize     = 1000
	walletTxOpTimeout     = 10 * time.Second
	walletTxCommitTimeout = 2 * time.Minute
)

func NewAtlasWallet(cfg *config.Config, logger *zap.Logger, clientCtx *client.Context, queryClients *types.QueryClients) (*AtlasWallet, error) {
	return NewAtlasWalletWithKeyName(cfg, logger, clientCtx, queryClients, "cygnus")
}

func NewAtlasWalletWithKeyName(cfg *config.Config, logger *zap.Logger, clientCtx *client.Context, queryClients *types.QueryClients, keyName string) (*AtlasWallet, error) {
	return NewAtlasWalletWithKeyNameAndSource(cfg, logger, clientCtx, queryClients, keyName, cfg.HomeDir)
}

func NewAtlasWalletWithKeyNameAndSource(cfg *config.Config, logger *zap.Logger, clientCtx *client.Context, queryClients *types.QueryClients, keyName, keySource string) (*AtlasWallet, error) {
	if strings.TrimSpace(keyName) == "" {
		return nil, fmt.Errorf("key name cannot be empty")
	}
	if strings.TrimSpace(keySource) == "" {
		keySource = cfg.HomeDir
	}
	keySource = os.ExpandEnv(keySource)

	gasPrices := cfg.ChainCfg.GasPrice
	if gasPrices == "" {
		gasPrices = config.DefaultChainConfig().GasPrice
	}
	if _, err := sdk.ParseDecCoins(gasPrices); err != nil {
		return nil, fmt.Errorf("invalid gas price %q: %w", gasPrices, err)
	}

	gasAdjustment := cfg.ChainCfg.GasAdjustment
	if gasAdjustment <= 0 {
		gasAdjustment = config.DefaultChainConfig().GasAdjustment
	}

	// Get encoding config from your blockchain
	encodingConfig := app.MakeEncodingConfig()

	// Create keyring
	kr, err := keyring.New(
		sdk.KeyringServiceName(),
		cfg.ChainCfg.KeyringBackend,
		keySource,
		os.Stdin,
		encodingConfig.Codec,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create keyring: %w", err)
	}

	info, err := kr.Key(keyName)
	if err != nil {
		return nil, err
	}
	address, err := info.GetAddress()
	if err != nil {
		return nil, err
	}

	walletClientCtx := clientCtx.
		WithKeyring(kr).
		WithFromName(keyName).
		WithFromAddress(address)

	clientCtx = &walletClientCtx

	// Initialize AtlasManager without GRPC connection first
	w := &AtlasWallet{
		logger:       logger,
		kr:           kr,
		clientCtx:    clientCtx,
		queryClients: queryClients,
		txClient:     sdktx.NewServiceClient(clientCtx.GRPCClient),

		keyName:       keyName,
		address:       address,
		gasPrices:     gasPrices,
		gasAdjustment: gasAdjustment,

		txQueue:         make(chan *walletTxRequest, walletTxQueueSize),
		highPriorityTxQ: make(chan *walletTxRequest, walletTxQueueSize),
		txStopChan:      make(chan struct{}),
	}

	if err := w.refreshAccountInfo(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to fetch initial account info: %w", err)
	}

	w.startTxQueue()

	return w, nil
}

// BroadcastTx broadcasts a transaction
func (w *AtlasWallet) BroadcastTxGrpc(retries int, wait bool, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	return w.BroadcastTxGrpcWithPriority(retries, wait, TxPriorityNormal, msgs...)
}

func (w *AtlasWallet) BroadcastTxGrpcHighPriority(retries int, wait bool, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	return w.BroadcastTxGrpcWithPriority(retries, wait, TxPriorityHigh, msgs...)
}

func (w *AtlasWallet) BroadcastTxGrpcWithPriority(retries int, wait bool, priority TxPriority, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	msgsCopy := append([]sdk.Msg(nil), msgs...)
	req := &walletTxRequest{
		retries: retries,
		wait:    wait,
		msgs:    msgsCopy,
		result:  make(chan walletTxResult, 1),
	}

	queue := w.txQueue
	if priority == TxPriorityHigh {
		queue = w.highPriorityTxQ
	}

	w.txQueueMu.RLock()
	if w.txStopped {
		w.txQueueMu.RUnlock()
		return nil, fmt.Errorf("wallet transaction queue stopped")
	}
	select {
	case queue <- req:
		w.txQueueMu.RUnlock()
	case <-w.txStopChan:
		w.txQueueMu.RUnlock()
		return nil, fmt.Errorf("wallet transaction queue stopped")
	}

	result := <-req.result
	return result.resp, result.err
}

func (w *AtlasWallet) startTxQueue() {
	w.txWG.Add(1)
	go w.processTxQueue()
}

func (w *AtlasWallet) Stop() {
	w.txStopOnce.Do(func() {
		w.txQueueMu.Lock()
		w.txStopped = true
		close(w.txStopChan)
		w.txQueueMu.Unlock()
		w.txWG.Wait()
	})
}

func (w *AtlasWallet) processTxQueue() {
	defer w.txWG.Done()

	for {
		select {
		case <-w.txStopChan:
			w.failQueuedTxs(fmt.Errorf("wallet transaction queue stopped"))
			return
		case req := <-w.highPriorityTxQ:
			w.handleQueuedTx(req)
		default:
		}

		select {
		case <-w.txStopChan:
			w.failQueuedTxs(fmt.Errorf("wallet transaction queue stopped"))
			return
		case req := <-w.highPriorityTxQ:
			w.handleQueuedTx(req)
		case req := <-w.txQueue:
			w.handleQueuedTx(req)
		}
	}
}

func (w *AtlasWallet) failQueuedTxs(err error) {
	w.failTxQueue(w.highPriorityTxQ, err)
	w.failTxQueue(w.txQueue, err)
}

func (w *AtlasWallet) failTxQueue(queue chan *walletTxRequest, err error) {
	for {
		select {
		case req := <-queue:
			req.result <- walletTxResult{err: err}
		default:
			return
		}
	}
}

func (w *AtlasWallet) handleQueuedTx(req *walletTxRequest) {
	w.broadcastQueuedTx(req)
}

func (w *AtlasWallet) broadcastQueuedTx(req *walletTxRequest) {
	var lastErr error

	for attempt := 0; attempt <= req.retries; attempt++ {
		if attempt > 0 {
			w.logger.Warn("Retrying queued transaction", zap.Int("attempt", attempt))
			time.Sleep(time.Duration(attempt) * time.Second)
		}

		ctx, cancel := context.WithTimeout(context.Background(), walletTxOpTimeout)
		txResp, err := w.signAndBroadcastOnce(ctx, req.msgs...)
		cancel()
		if err != nil {
			lastErr = err
			if w.isSequenceError(err) {
				w.logger.Warn("Sequence error detected, refreshing account info", zap.Error(err))
				if refreshErr := w.refreshAccountInfo(context.Background()); refreshErr != nil {
					w.logger.Error("Failed to refresh account info", zap.Error(refreshErr))
				}
			}
			continue
		}

		w.incrementSequence()

		if !req.wait {
			req.result <- walletTxResult{resp: txResp}
			return
		}

		confirmedResp, confirmErr := w.waitForTxWithTimeout(txResp.TxHash, walletTxCommitTimeout)
		if confirmErr != nil {
			w.logger.Error("Transaction did not confirm before queue continued",
				zap.String("tx_hash", txResp.TxHash),
				zap.Error(confirmErr))
			if refreshErr := w.refreshAccountInfo(context.Background()); refreshErr != nil {
				w.logger.Error("Failed to refresh account info after confirmation error", zap.Error(refreshErr))
			}
			if req.wait {
				req.result <- walletTxResult{resp: confirmedResp, err: confirmErr}
			}
			return
		}

		if req.wait {
			req.result <- walletTxResult{resp: confirmedResp}
		}
		return
	}

	if lastErr != nil {
		req.result <- walletTxResult{err: lastErr}
		return
	}
	req.result <- walletTxResult{err: fmt.Errorf("failed after %d retries", req.retries)}
}

func (w *AtlasWallet) signAndBroadcastOnce(ctx context.Context, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	accountNumber, sequence := w.accountInfo()

	// Create transaction factory with proper settings
	txf := tx.Factory{}.
		WithTxConfig(w.clientCtx.TxConfig).
		WithAccountRetriever(w.clientCtx.AccountRetriever).
		WithChainID(w.clientCtx.ChainID).
		WithGas(250000). // Default gas, will be adjusted by simulation
		WithGasAdjustment(w.gasAdjustment).
		WithGasPrices(w.gasPrices).
		WithKeybase(w.clientCtx.Keyring).
		WithAccountNumber(accountNumber).
		WithSequence(sequence).
		WithSignMode(signing.SignMode_SIGN_MODE_DIRECT).
		WithSimulateAndExecute(true).
		WithFromName(w.keyName)

	if w.clientCtx.GRPCClient == nil {
		return nil, fmt.Errorf("GRPC connection not established - cannot simulate gas")
	}

	_, adjusted, err := tx.CalculateGas(w.clientCtx, txf, msgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to simulate gas: %w", err)
	}

	// w.logger.Debug("Gas simulation result",
	// 	zap.Uint64("simulated_gas", simulatedGas.GasInfo.GasWanted),
	// 	zap.Uint64("adjusted_gas", adjusted),
	// 	zap.String("gas_prices", w.gasPrices),
	// 	zap.Uint64("sequence", sequence))

	txf = txf.WithGas(adjusted)

	// build unsigned transaction
	txb, err := txf.BuildUnsignedTx(msgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to build tx: %w", err)
	}

	// sign the transaction
	err = tx.Sign(ctx, txf, w.keyName, txb, true)
	if err != nil {
		return nil, fmt.Errorf("failed to sign tx: %w", err)
	}

	// Encode
	txBytes, err := w.clientCtx.TxConfig.TxEncoder()(txb.GetTx())
	if err != nil {
		return nil, fmt.Errorf("failed to encode tx: %w", err)
	}

	// Broadcast
	return w.broadcastTxBytes(ctx, txBytes, false)
}

// WaitForTx waits for transaction to be included in a block
func (w *AtlasWallet) WaitForTx(txHash string) (*sdk.TxResponse, error) {
	return w.waitForTxWithTimeout(txHash, walletTxOpTimeout)
}

func (w *AtlasWallet) WaitForTxWithTimeout(txHash string, timeout time.Duration) (*sdk.TxResponse, error) {
	return w.waitForTxWithTimeout(txHash, timeout)
}

func (w *AtlasWallet) waitForTxWithTimeout(txHash string, timeout time.Duration) (*sdk.TxResponse, error) {
	if w.txClient == nil {
		return nil, fmt.Errorf("tx client not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Warn("Timeout waiting for transaction",
				zap.String("tx_hash", txHash),
				zap.Duration("timeout", timeout))
			return nil, ctx.Err()

		case <-ticker.C:
			resp, err := w.txClient.GetTx(ctx, &sdktx.GetTxRequest{Hash: txHash})
			if err == nil {
				if resp.TxResponse.Code == 0 {
					w.logger.Info("Transaction confirmed",
						zap.String("tx_hash", txHash),
						zap.Int64("height", resp.TxResponse.Height))
					return resp.TxResponse, nil
				} else {
					return resp.TxResponse, fmt.Errorf("transaction failed: %s", resp.TxResponse.RawLog)
				}
			}
			// Transaction not found yet, continue waiting
		}
	}
}

func (w *AtlasWallet) GetSequence() uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.sequence
}

func (w *AtlasWallet) GetAddress() string {
	return w.address.String()
}

// broadcastTxBytes broadcasts encoded transaction bytes
func (w *AtlasWallet) broadcastTxBytes(ctx context.Context, txBytes []byte, wait bool) (*sdk.TxResponse, error) {
	if w.txClient == nil {
		return nil, fmt.Errorf("tx client not initialized")
	}

	// broadcast with sync mode
	broadcastReq := &sdktx.BroadcastTxRequest{
		TxBytes: txBytes,
		Mode:    sdktx.BroadcastMode_BROADCAST_MODE_SYNC,
	}

	broadcastResp, err := w.txClient.BroadcastTx(ctx, broadcastReq)
	if err != nil {
		return nil, fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	if broadcastResp.TxResponse.Code != 0 {
		return nil, fmt.Errorf("transaction failed: %s", broadcastResp.TxResponse.RawLog)
	}

	if wait {
		return w.WaitForTx(broadcastResp.TxResponse.TxHash)
	} else {
		return broadcastResp.TxResponse, nil
	}
}

// refreshAccountInfo fetches fresh account info from chain
func (w *AtlasWallet) refreshAccountInfo(ctx context.Context) error {
	accountNumber, sequence, err := w.queryAccountInfo(ctx)
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.accountNumber = accountNumber
	w.sequence = sequence
	w.mu.Unlock()

	w.logger.Debug("Refreshed account info",
		zap.Uint64("account_number", accountNumber),
		zap.Uint64("sequence", sequence))

	return nil
}

func (w *AtlasWallet) queryAccountInfo(ctx context.Context) (uint64, uint64, error) {
	resp, err := w.queryClients.Auth.Account(ctx, &authtypes.QueryAccountRequest{
		Address: w.address.String(),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to query account: %w", err)
	}

	var acc sdk.AccountI
	err = w.clientCtx.InterfaceRegistry.UnpackAny(resp.Account, &acc)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to unpack account: %w", err)
	}

	return acc.GetAccountNumber(), acc.GetSequence(), nil
}

func (w *AtlasWallet) accountInfo() (uint64, uint64) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.accountNumber, w.sequence
}

func (w *AtlasWallet) incrementSequence() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.sequence++
}

func (w *AtlasWallet) isSequenceError(err error) bool {
	errStr := err.Error()
	// CosmosSDK sequence error patterns
	sequenceErrors := []string{
		"invalid sequence",
		"wrong sequence",
		"sequence mismatch",
		"account sequence",
	}

	for _, seqErr := range sequenceErrors {
		if strings.Contains(errStr, seqErr) {
			return true
		}
	}
	return false
}
