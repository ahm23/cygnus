package wallet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdktx "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"

	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/rs/zerolog/log"

	storagetypes "nebulix/x/storage/types"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"

	"google.golang.org/grpc"

	// Import from your local blockchain
	"nebulix/app"

	"cygnus/types"
)

type AtlasManager struct {
	kr        keyring.Keyring
	clientCtx client.Context
	homeDir   string
	chainID   string
	grpcConn  *grpc.ClientConn
	keyName   string // Store the key name

	Address sdk.AccAddress
	// GRPC Clients
	QueryClient types.QueryClients
	txClient    sdktx.ServiceClient
}

// MsgClients groups all message clients
type MsgClients struct {
	Bank    banktypes.MsgClient
	Storage storagetypes.MsgClient
}

func NewAtlasManager(homeDir, name, chainID, keyringBackend, grpcEndpoint string) (*AtlasManager, error) {
	// Get encoding config from your blockchain
	encodingConfig := app.MakeEncodingConfig()

	// Create keyring
	kr, err := keyring.New(
		sdk.KeyringServiceName(),
		keyringBackend,
		homeDir,
		os.Stdin,
		encodingConfig.Codec,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create keyring: %w", err)
	}

	log.Info().Str("<NewAtlasManager> AtlasManager Name", name).Send()
	info, err := kr.Key(name)
	if err != nil {
		return nil, err
	}
	address, err := info.GetAddress()
	if err != nil {
		return nil, err
	}

	// Create client context
	clientCtx := client.Context{}.
		WithHomeDir(homeDir).
		WithChainID(chainID).
		WithKeyring(kr).
		WithInput(os.Stdin).
		WithOutput(os.Stdout).
		WithCodec(encodingConfig.Codec).
		WithInterfaceRegistry(encodingConfig.InterfaceRegistry).
		WithTxConfig(encodingConfig.TxConfig).
		WithLegacyAmino(encodingConfig.Amino).
		WithBroadcastMode("sync").
		WithSkipConfirmation(true).
		WithFromName(name).
		WithFromAddress(address).
		WithSignModeStr("direct")

	registerAccountInterfaces(clientCtx.InterfaceRegistry)

	// Initialize AtlasManager without GRPC connection first
	w := &AtlasManager{
		kr:        kr,
		clientCtx: clientCtx,
		homeDir:   homeDir,
		chainID:   chainID,
		Address:   address,
		keyName:   name,
	}

	// Connect to GRPC if endpoint provided
	if grpcEndpoint != "" {
		if err := w.ConnectGRPC(grpcEndpoint); err != nil {
			return nil, fmt.Errorf("failed to connect to GRPC: %w", err)
		}
	}

	return w, nil
}

// ConnectGRPC establishes GRPC connection and initializes clients
func (w *AtlasManager) ConnectGRPC(endpoint string) error {
	// Create GRPC connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(
		ctx,
		endpoint,
		grpc.WithInsecure(), // Use grpc.WithTransportCredentials(insecure.NewCredentials()) for newer grpc
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to GRPC endpoint: %w", err)
	}

	w.grpcConn = conn

	w.clientCtx = w.clientCtx.WithGRPCClient(conn)

	// Initialize query clients
	w.QueryClient = types.QueryClients{
		Auth:    authtypes.NewQueryClient(conn),
		Bank:    banktypes.NewQueryClient(conn),
		Storage: storagetypes.NewQueryClient(conn),
	}

	w.txClient = sdktx.NewServiceClient(conn)

	return nil
}

// Close closes the GRPC connection
func (w *AtlasManager) Close() error {
	if w.grpcConn != nil {
		return w.grpcConn.Close()
	}
	return nil
}

// GetAddress returns address for a key
func (w *AtlasManager) GetAddress(keyName string) (sdk.AccAddress, error) {
	info, err := w.kr.Key(keyName)
	if err != nil {
		return nil, err
	}
	return info.GetAddress()
}

// GetAccountInfo gets account number and sequence
func (w *AtlasManager) GetAccountInfo() (accountNum, sequence uint64, err error) {
	// Query account information
	ctx := context.Background()
	resp, err := w.QueryClient.Auth.Account(ctx, &authtypes.QueryAccountRequest{
		Address: w.Address.String(),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to query account: %w", err)
	}

	// Unmarshal the account interface
	var acc sdk.AccountI
	err = w.clientCtx.InterfaceRegistry.UnpackAny(resp.Account, &acc)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to unpack account: %w", err)
	}

	return acc.GetAccountNumber(), acc.GetSequence(), nil
}

// BroadcastTx broadcasts a transaction
func (w *AtlasManager) BroadcastTxGrpc(msgs ...sdk.Msg) (*sdk.TxResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Get account info first
	accountNum, sequence, err := w.GetAccountInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get account info: %w", err)
	}

	// Create transaction factory with proper settings
	txf := tx.Factory{}.
		WithTxConfig(w.clientCtx.TxConfig).
		WithAccountRetriever(w.clientCtx.AccountRetriever).
		WithChainID(w.chainID).
		WithGas(200000). // Default gas, will be adjusted by simulation
		WithGasAdjustment(1.2).
		WithFees("2000unblx"). // Adjust based on your chain
		WithKeybase(w.clientCtx.Keyring).
		WithSignMode(signing.SignMode_SIGN_MODE_DIRECT).
		WithSimulateAndExecute(true).
		WithAccountNumber(accountNum).
		WithSequence(sequence).
		WithFromName("cygnus")

	if w.grpcConn == nil {
		return nil, fmt.Errorf("GRPC connection not established - cannot simulate gas")
	}

	// // Build and simulate first
	// _, adjusted, err := tx.CalculateGas(w.clientCtx, txf, msgs...)
	// if err != nil {
	// 	return nil, fmt.Errorf("failed to simulate gas: %w", err)
	// }

	// // Update with actual gas
	// txf = txf.WithGas(adjusted)

	// Build unsigned transaction
	txb, err := txf.BuildUnsignedTx(msgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to build tx: %w", err)
	}

	// Sign the transaction
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
	return w.broadcastTxBytes(txBytes)
}

// broadcastTxBytes broadcasts encoded transaction bytes
func (w *AtlasManager) broadcastTxBytes(txBytes []byte) (*sdk.TxResponse, error) {
	if w.txClient == nil {
		return nil, fmt.Errorf("tx client not initialized")
	}

	// Broadcast with sync mode
	broadcastReq := &sdktx.BroadcastTxRequest{
		TxBytes: txBytes,
		Mode:    sdktx.BroadcastMode_BROADCAST_MODE_SYNC,
	}

	broadcastResp, err := w.txClient.BroadcastTx(context.Background(), broadcastReq)
	if err != nil {
		return nil, fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	fmt.Println("TxReponse Code:", broadcastResp.TxResponse.Code)

	txResponse, err := w.WaitForTx(broadcastResp.TxResponse.TxHash)
	if err != nil {
		return nil, err
	} else if txResponse.Code != 0 {
		return nil, errors.New(txResponse.RawLog)
	} else {
		return broadcastResp.TxResponse, nil
	}
}

// WaitForTx waits for transaction to be included in a block
func (w *AtlasManager) WaitForTx(txHash string) (*sdk.TxResponse, error) {
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
			fmt.Println("timeout waiting for transaction", txHash)
			return nil, ctx.Err()

		case <-ticker.C:
			resp, err := w.txClient.GetTx(context.Background(), &sdktx.GetTxRequest{Hash: txHash})
			if err == nil {
				return resp.TxResponse, nil // tx found (executed)
			}
		}
	}
}
