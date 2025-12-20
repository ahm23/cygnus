package atlas

import (
	"context"
	"fmt"
	"sync"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"go.uber.org/zap"
)

// TransactionSequencer ensures ordered transaction submission
type TransactionSequencer struct {
	wallet     *AtlasWallet
	logger     *zap.Logger
	txQueue    chan *QueuedTransaction
	stopChan   chan struct{}
	wg         sync.WaitGroup
	mu         sync.Mutex
	pendingTxs map[uint64]*PendingTx
}

type QueuedTransaction struct {
	Type      string
	Msg       sdk.Msg
	Callback  func(txHash string, err error)
	CreatedAt time.Time
}

type PendingTx struct {
	Sequence  uint64
	TxHash    string
	Submitted bool
	Confirmed bool
	Error     error
	Callback  func(txHash string, err error)
}

func NewTransactionSequencer(wallet *AtlasWallet, logger *zap.Logger) *TransactionSequencer {
	return &TransactionSequencer{
		wallet:     wallet,
		logger:     logger,
		txQueue:    make(chan *QueuedTransaction, 1000),
		stopChan:   make(chan struct{}),
		pendingTxs: make(map[uint64]*PendingTx),
	}
}

func (ts *TransactionSequencer) Start() {
	ts.logger.Info("Starting transaction sequencer")

	ts.wg.Add(2)
	go ts.processQueue()
	go ts.monitorConfirmations()
}

func (ts *TransactionSequencer) Stop() {
	close(ts.stopChan)
	ts.wg.Wait()
	ts.logger.Info("Transaction sequencer stopped")
}

// StageFile enqueues a StageFile transaction for ordered processing
func (ts *TransactionSequencer) SubmitTx(ctx context.Context, txType string, msg sdk.Msg) (string, error) {
	resultChan := make(chan struct {
		txHash string
		err    error
	}, 1)

	queuedTx := &QueuedTransaction{
		Type: txType,
		Msg:  msg,
		Callback: func(txHash string, err error) {
			resultChan <- struct {
				txHash string
				err    error
			}{txHash, err}
		},
		CreatedAt: time.Now(),
	}

	// Enqueue for ordered processing
	select {
	case ts.txQueue <- queuedTx:
		ts.logger.Info("transaction queued", zap.String("tx_type", txType))
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Wait for result
	select {
	case result := <-resultChan:
		return result.txHash, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (ts *TransactionSequencer) processQueue() {
	defer ts.wg.Done()

	for {
		select {
		case <-ts.stopChan:
			return
		case queuedTx := <-ts.txQueue:
			ts.processTransaction(queuedTx)
		}
	}
}

func (ts *TransactionSequencer) processTransaction(queuedTx *QueuedTransaction) {
	ts.mu.Lock()
	sequence := ts.wallet.GetSequence() + 1

	ts.logger.Info("Processing queued transaction",
		zap.String("type", queuedTx.Type),
		zap.Uint64("sequence", sequence))

	// Store as pending
	ts.pendingTxs[sequence] = &PendingTx{
		Sequence: sequence,
		Callback: queuedTx.Callback,
	}

	// brodcast transaction
	txResp, err := ts.wallet.BroadcastTxGrpc(0, false, queuedTx.Msg)
	if err != nil {
		ts.handleTxValidationError(sequence, err, queuedTx.Callback)
		return
	}
	ts.mu.Unlock() // DEV NOTE: tx pre-checks/validation passed; mutex on sequence can be freed

	txResp, err = ts.wallet.WaitForTx(txResp.TxHash)
	if err != nil {
		ts.handleTxError(sequence, err, queuedTx.Callback)
		return
	}

	// update pending tx
	ts.mu.Lock()
	if pending, exists := ts.pendingTxs[sequence]; exists {
		pending.TxHash = txResp.TxHash
		pending.Submitted = true
	}
	ts.mu.Unlock()
}

func (ts *TransactionSequencer) handleTxError(sequence uint64, err error, callback func(string, error)) {
	ts.logger.Error("Transaction failed",
		zap.Uint64("sequence", sequence),
		zap.Error(err))

	delete(ts.pendingTxs, sequence)

	if callback != nil {
		callback("", err)
	}
}

func (ts *TransactionSequencer) handleTxValidationError(sequence uint64, err error, callback func(string, error)) {
	ts.logger.Error("Transaction validation failed",
		zap.Uint64("sequence", sequence),
		zap.Error(err))

	delete(ts.pendingTxs, sequence)
	ts.mu.Unlock()

	if callback != nil {
		callback("", err)
	}
}

func (ts *TransactionSequencer) monitorConfirmations() {
	defer ts.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ts.stopChan:
			return
		case <-ticker.C:
			ts.checkPendingConfirmations()
		}
	}
}

func (ts *TransactionSequencer) checkPendingConfirmations() {
	ts.mu.Lock()
	pendingSeqs := make([]uint64, 0, len(ts.pendingTxs))
	for seq, tx := range ts.pendingTxs {
		if tx.Submitted && !tx.Confirmed {
			pendingSeqs = append(pendingSeqs, seq)
		}
	}
	ts.mu.Unlock()

	for _, seq := range pendingSeqs {
		ts.checkTxConfirmation(seq)
	}
}

func (ts *TransactionSequencer) checkTxConfirmation(sequence uint64) {
	ts.mu.Lock()
	pendingTx, exists := ts.pendingTxs[sequence]
	ts.mu.Unlock()

	if !exists || pendingTx.Confirmed {
		return
	}

	// Check transaction status
	txResp, err := ts.wallet.WaitForTx(pendingTx.TxHash)
	if err != nil {
		// Still pending or error
		return
	}

	// Transaction confirmed
	ts.mu.Lock()
	pendingTx.Confirmed = true
	pendingTx.Error = nil
	if txResp.Code != 0 {
		pendingTx.Error = fmt.Errorf("transaction failed: %s", txResp.RawLog)
	}
	ts.mu.Unlock()

	// Execute callback
	if pendingTx.Callback != nil {
		pendingTx.Callback(pendingTx.TxHash, pendingTx.Error)
	}

	// Clean up after some time
	go func(seq uint64) {
		time.Sleep(5 * time.Minute)
		ts.mu.Lock()
		delete(ts.pendingTxs, seq)
		ts.mu.Unlock()
	}(sequence)
}
