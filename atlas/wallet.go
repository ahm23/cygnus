package atlas

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdktx "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/rs/zerolog/log"

	"github.com/cosmos/cosmos-sdk/crypto/keyring"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"

	// Import from your local blockchain
	"atlas/app"

	"cygnus/config"
	"cygnus/types"
)

type AtlasWallet struct {
	mu sync.RWMutex

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

	txQueue          chan *walletTx
	txQueueExpedited chan *walletTx
	txStopChan       chan struct{}
	txWG             sync.WaitGroup
	txStopOnce       sync.Once
	txQueueMu        sync.RWMutex
	txStopped        bool
	normalTxPause    atomic.Int64
}

type walletTx struct {
	retries   int
	wait      bool
	gasPrices string // "" means use wallet default
	msgs      []sdk.Msg
	result    chan walletTxResult
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

func NewAtlasWallet(cfg *config.Config, clientCtx *client.Context, queryClients *types.QueryClients, keyName, keySource string) (*AtlasWallet, error) {
	log.Debug().Str("name", keyName).Msg("Initializing wallet...")
	if strings.TrimSpace(keyName) == "" {
		return nil, fmt.Errorf("key name cannot be empty")
	}
	if strings.TrimSpace(keySource) == "" {
		keySource = cfg.HomeDirectory
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

	log.Debug().
		Str("source", keySource).
		Str("backend", cfg.ChainCfg.KeyringBackend).
		Msg("Creating keyring...")

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
	log.Debug().
		Str("address", address.String()).
		Str("pubkey", info.PubKey.GoString()).
		Msg("Key info: ")

	walletClientCtx := clientCtx.
		WithKeyring(kr).
		WithFromName(keyName).
		WithFromAddress(address)

	clientCtx = &walletClientCtx

	// Initialize AtlasManager without GRPC connection first
	w := &AtlasWallet{
		kr:           kr,
		clientCtx:    clientCtx,
		queryClients: queryClients,
		txClient:     sdktx.NewServiceClient(clientCtx.GRPCClient),

		keyName:       keyName,
		address:       address,
		gasPrices:     gasPrices,
		gasAdjustment: gasAdjustment,

		txQueue:          make(chan *walletTx, walletTxQueueSize),
		txQueueExpedited: make(chan *walletTx, walletTxQueueSize),
		txStopChan:       make(chan struct{}),
	}

	if err := w.refreshAccountInfo(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to fetch initial account info: %w", err)
	}

	w.txWG.Add(1)
	go w.processTxQueue()

	return w, nil
}

// GetSequence gets the wallet's sequence number
func (w *AtlasWallet) GetSequence() uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.sequence
}

// GetAddress gets the wallet's address
func (w *AtlasWallet) GetAddress() string {
	return w.address.String()
}

// BroadcastTx broadcasts a transaction
func (w *AtlasWallet) BroadcastTxGrpc(retries int, wait bool, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	return w.executeTx(retries, wait, false, "", msgs...)
}

// BroadcastExpeditedTxGrpc broadcasts a transaction with priority over all other standard transactions to be submitted.
func (w *AtlasWallet) BroadcastExpeditedTxGrpc(retries int, wait bool, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	return w.executeTx(retries, wait, true, "", msgs...)
}

// BroadcastProofTxGrpc broadcasts a proof transaction with zero gas price.
// Registered storage providers are exempt from fees on MsgProveFile transactions.
func (w *AtlasWallet) BroadcastProofTxGrpc(retries int, wait bool, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	return w.executeTx(retries, wait, false, "0uatl", msgs...)
}

// BroadcastProofExpeditedTxGrpc broadcasts an expedited proof transaction with zero gas price.
func (w *AtlasWallet) BroadcastProofExpeditedTxGrpc(retries int, wait bool, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	return w.executeTx(retries, wait, true, "0uatl", msgs...)
}

// WaitForTx waits for transaction to be included in a block.
func (w *AtlasWallet) WaitForTx(txHash string) (*sdk.TxResponse, error) {
	return w.waitForTx(txHash, walletTxOpTimeout)
}

// WaitForTxWithTimeout waits for transaction to be included in a block.
func (w *AtlasWallet) WaitForTxWithTimeout(txHash string, timeout time.Duration) (*sdk.TxResponse, error) {
	return w.waitForTx(txHash, timeout)
}

// Stop sends a stop signal to all active wallet goroutines such as the transaction queue poller.
// Consequently, it also clears all queued transactions.
func (w *AtlasWallet) Stop() {
	w.txStopOnce.Do(func() {
		w.txQueueMu.Lock()
		w.txStopped = true
		close(w.txStopChan)
		w.txQueueMu.Unlock()
		w.txWG.Wait()
	})
}

// executeTx broadcasts a tx and provides options to retry the tx a given number of times,
// wait for the tx to be included in a block, and expedite the tx.
func (w *AtlasWallet) executeTx(retries int, wait bool, expedite bool, gasPrices string, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	msgsCopy := append([]sdk.Msg(nil), msgs...)
	tx := &walletTx{
		retries:   retries,
		wait:      wait,
		gasPrices: gasPrices,
		msgs:      msgsCopy,
		result:    make(chan walletTxResult, 1),
	}

	// select standard or expedited queue
	queue := w.txQueue
	if expedite {
		queue = w.txQueueExpedited
	}

	// check for stop signal
	select {
	case <-w.txStopChan:
		return nil, fmt.Errorf("wallet transaction queue stopped")
	default:
	}

	// add tx to queue
	select {
	case queue <- tx:
	case <-w.txStopChan:
		return nil, fmt.Errorf("wallet transaction queue stopped")
	}

	// wait for tx result
	result := <-tx.result
	return result.resp, result.err
}

// processTxQueue will continously poll for new transactions in the txQueue channels and handle them.
// The polling can be stopped using the stop signal channel.
func (w *AtlasWallet) processTxQueue() {
	defer w.txWG.Done()
	defer w.failQueuedTxs(fmt.Errorf("wallet transaction queue stopped"))

	for {
		// priority to expedited
		select {
		case <-w.txStopChan:
			return
		case req := <-w.txQueueExpedited:
			w.handleQueuedTx(req)
			continue // Loop back to check for more expedited
		default:
		}

		// process normal if no expedited available
		select {
		case <-w.txStopChan:
			return
		case req := <-w.txQueueExpedited: //
			w.handleQueuedTx(req)
		case req := <-w.txQueue:
			w.handleQueuedTx(req)
		}
	}
}

// failQueuedTxs fails all transactions queued in the standard and expedited queues.
func (w *AtlasWallet) failQueuedTxs(err error) {
	w.failTxQueue(w.txQueueExpedited, err)
	w.failTxQueue(w.txQueue, err)
}

// failTxQueue fails all the transactions in a given queue.
func (w *AtlasWallet) failTxQueue(queue chan *walletTx, err error) {
	for {
		select {
		case req := <-queue:
			req.result <- walletTxResult{err: err}
		default:
			return
		}
	}
}

// handleQueuedTx submits the request messages for signing and broadcasting
// and waits for block inclusion if specified by the walletTx object.
func (w *AtlasWallet) handleQueuedTx(req *walletTx) {
	var err error
	var txResp *sdk.TxResponse

	for attempt := 0; attempt <= req.retries; attempt++ {

		// on retry, pause for 1-3 seconds and refresh account sequence number
		if attempt > 0 {
			log.Error().
				Str("tx_hash", txResp.TxHash).
				Err(err).
				Msg("Transaction failed")

			time.Sleep(time.Duration(attempt) * time.Second)
			log.Debug().Int("attempt", attempt).Msg("Retrying queued transaction")

			if refreshErr := w.refreshAccountInfo(context.Background()); refreshErr != nil {
				log.Error().Err(refreshErr).Msg("Failed to refresh account info")
			}
		}

		// broadcast transaction
		ctx, cancel := context.WithTimeout(context.Background(), walletTxOpTimeout)
		txResp, err = w.signAndBroadcastTx(ctx, req.gasPrices, req.msgs...)
		cancel()
		if err != nil {
			continue
		}

		// increment local sequence number on successful broadcast
		w.mu.Lock()
		w.sequence++
		w.mu.Unlock()

		// if wait flag is true, wait for transaction block inclusion for real response
		if req.wait {
			txResp, err = w.waitForTx(txResp.TxHash, walletTxCommitTimeout)
			if err != nil {
				continue
			}
		}

		// trigger transaction completion in result channel, with no error since everything passed
		req.result <- walletTxResult{resp: txResp, err: nil}
		return
	}

	// trigger transaction completion in result channel, including the error that caused the last attempt to fail
	req.result <- walletTxResult{resp: txResp, err: err}
}

// signAndBroadcastTx simulates the required gas, then builds, signs, encodes, and broadcasts the transaction.
// If gasPricesOverride is non-empty, it is used instead of the wallet's configured gas price.
func (w *AtlasWallet) signAndBroadcastTx(ctx context.Context, gasPricesOverride string, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	accountNumber, sequence := w.accountInfo()

	gasPrices := w.gasPrices
	if gasPricesOverride != "" {
		gasPrices = gasPricesOverride
	}

	// create transaction factory
	txf := tx.Factory{}.
		WithTxConfig(w.clientCtx.TxConfig).
		WithAccountRetriever(w.clientCtx.AccountRetriever).
		WithChainID(w.clientCtx.ChainID).
		WithGas(250000). // default gas, though gas will be adjusted by simulation
		WithGasAdjustment(w.gasAdjustment).
		WithGasPrices(gasPrices).
		WithKeybase(w.clientCtx.Keyring).
		WithAccountNumber(accountNumber).
		WithSequence(sequence).
		WithSignMode(signing.SignMode_SIGN_MODE_DIRECT).
		WithSimulateAndExecute(true).
		WithFromName(w.keyName)

	if w.clientCtx.GRPCClient == nil {
		return nil, fmt.Errorf("GRPC connection not established - cannot simulate gas")
	}

	// determine gas required
	_, adjusted, err := tx.CalculateGas(w.clientCtx, txf, msgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to simulate gas: %w", err)
	}
	txf = txf.WithGas(adjusted)

	// build unsigned transaction
	txb, err := txf.BuildUnsignedTx(msgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to build tx: %w", err)
	}

	// sign
	err = tx.Sign(ctx, txf, w.keyName, txb, true)
	if err != nil {
		return nil, fmt.Errorf("failed to sign tx: %w", err)
	}

	// encode
	txBytes, err := w.clientCtx.TxConfig.TxEncoder()(txb.GetTx())
	if err != nil {
		return nil, fmt.Errorf("failed to encode tx: %w", err)
	}

	// broadcast
	return w.broadcastTxBytes(ctx, txBytes)
}

// broadcastTxBytes broadcasts encoded transaction bytes.
func (w *AtlasWallet) broadcastTxBytes(ctx context.Context, txBytes []byte) (*sdk.TxResponse, error) {
	if w.txClient == nil {
		return nil, fmt.Errorf("tx client not initialized")
	}

	// configure broadcast with sync mode
	broadcastReq := &sdktx.BroadcastTxRequest{
		TxBytes: txBytes,
		Mode:    sdktx.BroadcastMode_BROADCAST_MODE_SYNC,
	}

	// broadcast transaction
	broadcastResp, err := w.txClient.BroadcastTx(ctx, broadcastReq)
	if err != nil {
		return nil, fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	// return error on non-zero response code
	if broadcastResp.TxResponse.Code != 0 {
		return broadcastResp.TxResponse, fmt.Errorf("transaction failed (%d): %s", broadcastResp.TxResponse.Code, broadcastResp.TxResponse.RawLog)
	}

	return broadcastResp.TxResponse, nil
}

// waitForTx
func (w *AtlasWallet) waitForTx(txHash string, timeout time.Duration) (*sdk.TxResponse, error) {
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
			log.Warn().
				Str("tx_hash", txHash).
				Dur("timeout", timeout).
				Msg("Timeout waiting for transaction")
			return nil, ctx.Err()

		case <-ticker.C:
			resp, err := w.txClient.GetTx(ctx, &sdktx.GetTxRequest{Hash: txHash})
			if err == nil {
				if resp.TxResponse.Code == 0 {
					log.Info().
						Str("tx_hash", txHash).
						Int64("height", resp.TxResponse.Height).
						Msg("Transaction confirmed")
					return resp.TxResponse, nil
				} else {
					return resp.TxResponse, fmt.Errorf("transaction failed: %s", resp.TxResponse.RawLog)
				}
			}
			// Transaction not found yet, continue waiting
		}
	}
}

// refreshAccountInfo queries and refreshes account info.
func (w *AtlasWallet) refreshAccountInfo(ctx context.Context) error {
	log.Debug().Msg("Refreshing account info...")
	accountNumber, sequence, err := w.queryAccountInfo(ctx)
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.accountNumber = accountNumber
	w.sequence = sequence
	w.mu.Unlock()

	log.Debug().
		Uint64("account_number", accountNumber).
		Uint64("sequence", sequence).
		Msg("Refreshed account info")

	return nil
}

// queryAccountInfo fetches the latest account info from chain.
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

// accountInfo is an atomic account info getter.
func (w *AtlasWallet) accountInfo() (uint64, uint64) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.accountNumber, w.sequence
}
