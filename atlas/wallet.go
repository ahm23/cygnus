package atlas

import (
	"context"
	"fmt"
	"os"
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
	"nebulix/app"

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

	address       sdk.Address
	accountNumber uint64
	sequence      uint64
}

func NewAtlasWallet(cfg *config.Config, logger *zap.Logger, clientCtx *client.Context, queryClients *types.QueryClients) (*AtlasWallet, error) {
	// Get encoding config from your blockchain
	encodingConfig := app.MakeEncodingConfig()

	// Create keyring
	kr, err := keyring.New(
		sdk.KeyringServiceName(),
		cfg.ChainCfg.KeyringBackend,
		cfg.HomeDir,
		os.Stdin,
		encodingConfig.Codec,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create keyring: %w", err)
	}

	info, err := kr.Key("cygnus")
	if err != nil {
		return nil, err
	}
	address, err := info.GetAddress()
	if err != nil {
		return nil, err
	}

	walletClientCtx := clientCtx.
		WithKeyring(kr).
		WithFromName("cygnus").
		WithFromAddress(address)

	clientCtx = &walletClientCtx

	// Initialize AtlasManager without GRPC connection first
	w := &AtlasWallet{
		logger:       logger,
		kr:           kr,
		clientCtx:    clientCtx,
		queryClients: queryClients,
		txClient:     sdktx.NewServiceClient(clientCtx.GRPCClient),

		address: address,
	}

	if err := w.refreshAccountInfo(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to fetch initial account info: %w", err)
	}

	return w, nil
}

// BroadcastTx broadcasts a transaction
func (w *AtlasWallet) BroadcastTxGrpc(retries int, wait bool, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// get current sequence
	sequence, err := w.getNextSequence()
	if err != nil {
		return nil, fmt.Errorf("failed to get sequence: %w", err)
	}

	// Create transaction factory with proper settings
	txf := tx.Factory{}.
		WithTxConfig(w.clientCtx.TxConfig).
		WithAccountRetriever(w.clientCtx.AccountRetriever).
		WithChainID(w.clientCtx.ChainID).
		WithGas(200000). // Default gas, will be adjusted by simulation
		WithGasAdjustment(1.2).
		WithFees("2000unblx"). // Adjust based on your chain
		WithKeybase(w.clientCtx.Keyring).
		WithSignMode(signing.SignMode_SIGN_MODE_DIRECT).
		WithSimulateAndExecute(true).
		WithAccountNumber(w.accountNumber).
		WithSequence(sequence).
		WithFromName("cygnus")

	if w.clientCtx.GRPCClient == nil {
		return nil, fmt.Errorf("GRPC connection not established - cannot simulate gas")
	}

	// simulate and update gas
	simulatedGas, adjusted, err := tx.CalculateGas(w.clientCtx, txf, msgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to simulate gas: %w", err)
	}

	w.logger.Debug("Gas simulation result",
		zap.Uint64("simulated_gas", simulatedGas.GasInfo.GasWanted),
		zap.Uint64("adjusted_gas", adjusted))

	txf = txf.WithGas(adjusted)

	// build unsigned transaction
	txb, err := txf.BuildUnsignedTx(msgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to build tx: %w", err)
	}

	// sign the transaction
	err = tx.Sign(ctx, txf, "cygnus", txb, true)
	if err != nil {
		return nil, fmt.Errorf("failed to sign tx: %w", err)
	}

	// Encode
	txBytes, err := w.clientCtx.TxConfig.TxEncoder()(txb.GetTx())
	if err != nil {
		return nil, fmt.Errorf("failed to encode tx: %w", err)
	}

	// Broadcast
	return w.broadcastWithRetry(ctx, txBytes, retries, wait)
}

// WaitForTx waits for transaction to be included in a block
func (w *AtlasWallet) WaitForTx(txHash string) (*sdk.TxResponse, error) {
	if w.txClient == nil {
		return nil, fmt.Errorf("tx client not initialized")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Warn("Timeout waiting for transaction",
				zap.String("tx_hash", txHash),
				zap.Duration("timeout", 10*time.Second))
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
	return w.sequence
}

// broadcastWithRetry handles broadcasting with retry for sequence errors
func (w *AtlasWallet) broadcastWithRetry(ctx context.Context, txBytes []byte, retries int, wait bool) (*sdk.TxResponse, error) {

	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			w.logger.Warn("Retrying transaction broadcast", zap.Int("attempt", attempt))
			time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
		}

		txResp, err := w.broadcastTxBytes(ctx, txBytes, wait)
		if err == nil {
			return txResp, nil
		}

		// Check if it's a sequence error
		if w.isSequenceError(err) {
			w.logger.Warn("Sequence error detected, refreshing account info")

			// Refresh account info and get new sequence
			if refreshErr := w.refreshAccountInfo(ctx); refreshErr != nil {
				w.logger.Error("Failed to refresh account info", zap.Error(refreshErr))
			}

			// Non-retryable error
			return nil, err
		}
	}

	return nil, fmt.Errorf("failed after %d retries", retries)
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
	resp, err := w.queryClients.Auth.Account(ctx, &authtypes.QueryAccountRequest{
		Address: w.address.String(),
	})
	if err != nil {
		return fmt.Errorf("failed to query account: %w", err)
	}

	var acc sdk.AccountI
	err = w.clientCtx.InterfaceRegistry.UnpackAny(resp.Account, &acc)
	if err != nil {
		return fmt.Errorf("failed to unpack account: %w", err)
	}

	w.mu.Lock()
	w.accountNumber = acc.GetAccountNumber()
	w.sequence = acc.GetSequence()
	w.mu.Unlock()

	w.logger.Debug("Refreshed account info",
		zap.Uint64("account_number", w.accountNumber),
		zap.Uint64("sequence", w.sequence))

	return nil
}

// getNextSequence returns the next sequence to use
func (w *AtlasWallet) getNextSequence() (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	sequence := w.sequence
	w.sequence++

	return sequence, nil
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
		if contains(errStr, seqErr) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || contains(s[1:], substr)))
}
