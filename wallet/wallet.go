package wallet

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"

	storagetypes "nebulix/x/storage/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"

	"google.golang.org/grpc"

	// Import from your local blockchain
	"nebulix/app"
)

type Wallet struct {
	kr        keyring.Keyring
	clientCtx client.Context
	homeDir   string
	chainID   string
	grpcConn  *grpc.ClientConn

	Address sdk.Address
	// GRPC Clients
	QueryClient QueryClients
	MsgClient   MsgClients
}

// QueryClients groups all query clients
type QueryClients struct {
	Auth    authtypes.QueryClient
	Bank    banktypes.QueryClient
	Storage storagetypes.QueryClient
}

// MsgClients groups all message clients
type MsgClients struct {
	Bank    banktypes.MsgClient
	Storage storagetypes.MsgClient
}

func NewWallet(homeDir, name, chainID, keyringBackend, grpcEndpoint string) (*Wallet, error) {
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
		WithSkipConfirmation(true)

	// Initialize wallet without GRPC connection first
	w := &Wallet{
		Address:   address,
		kr:        kr,
		clientCtx: clientCtx,
		homeDir:   homeDir,
		chainID:   chainID,
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
func (w *Wallet) ConnectGRPC(endpoint string) error {
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

	// Initialize query clients
	w.QueryClient = QueryClients{
		Auth:    authtypes.NewQueryClient(conn),
		Bank:    banktypes.NewQueryClient(conn),
		Storage: storagetypes.NewQueryClient(conn),
	}

	// Initialize msg clients
	w.MsgClient = MsgClients{
		Bank:    banktypes.NewMsgClient(conn),
		Storage: storagetypes.NewMsgClient(conn),
	}

	return nil
}

// Close closes the GRPC connection
func (w *Wallet) Close() error {
	if w.grpcConn != nil {
		return w.grpcConn.Close()
	}
	return nil
}

// GetAddress returns address for a key
func (w *Wallet) GetAddress(keyName string) (sdk.AccAddress, error) {
	info, err := w.kr.Key(keyName)
	if err != nil {
		return nil, err
	}
	return info.GetAddress()
}

// // BroadcastTx broadcasts a transaction
// func BroadcastTxGrpc(clientCtx client.Context, msgs ...sdk.Msg) (*sdk.TxResponse, error) {
// 	txf, err := tx.NewFactoryCLI(clientCtx, nil)
// 	if err != nil {
// 		return nil, err
// 	}
// 	txf, err = prepareFactory(clientCtx, txf)
// 	if err != nil {
// 		return nil, err
// 	}

// 	// calculate gas
// 	if txf.SimulateAndExecute() || clientCtx.Simulate {
// 		_, adjusted, err := tx.CalculateGas(clientCtx, txf, msgs...)
// 		if err != nil {
// 			return nil, err
// 		}

// 		txf = txf.WithGas(adjusted)
// 		_, _ = fmt.Fprintf(os.Stderr, "%s\n", tx.GasEstimateResponse{GasEstimate: txf.Gas()})
// 	}

// 	// build transaction
// 	txn, err := txf.BuildUnsignedTx(msgs...)
// 	if err != nil {
// 		return nil, err
// 	}

// 	// log transaction
// 	out, err := clientCtx.TxConfig.TxJSONEncoder()(txn.GetTx())
// 	if err != nil {
// 		return nil, err
// 	}
// 	_, _ = fmt.Fprintf(os.Stderr, "%s\n\n", out)

// 	if clientCtx.Simulate {
// 		return nil, nil
// 	}

// 	// set fee granter
// 	txn.SetFeeGranter(clientCtx.GetFeeGranterAddress())

// 	// sign transaction
// 	err = tx.Sign(clientCtx.CmdContext, txf, clientCtx.GetFromName(), txn, true)
// 	if err != nil {
// 		return nil, err
// 	}

// 	// prepare transaction bytes
// 	txBytes, err := clientCtx.TxConfig.TxEncoder()(txn.GetTx())
// 	if err != nil {
// 		return nil, err
// 	}

// 	// Broadcast transaction using gRPC
// 	ctx := context.Background()
// 	broadcastReq := &sdktx.BroadcastTxRequest{
// 		TxBytes: txBytes,
// 		Mode:    sdktx.BroadcastMode_BROADCAST_MODE_SYNC, // Use SYNC for confirmation of tx delivery
// 	}
// 	// broadcastResp, err := txClient.BroadcastTx(ctx, broadcastReq)
// 	// if err != nil {
// 	// 	return nil, fmt.Errorf("failed to broadcast transaction: %w", err)
// 	// }

// 	// // Convert gRPC response to sdk.TxResponse
// 	// sdkResp := &sdk.TxResponse{
// 	// 	TxHash: broadcastResp.TxResponse.TxHash,
// 	// 	Code:   broadcastResp.TxResponse.Code,
// 	// 	Data:   broadcastResp.TxResponse.Data,
// 	// 	RawLog: broadcastResp.TxResponse.RawLog,
// 	// 	Logs:   sdk.ABCIMessageLogs{}, // Parse logs if needed
// 	// }

// 	// if sdkResp.Code != 0 {
// 	// 	fmt.Errorf(sdkResp.String())
// 	// 	return sdkResp, fmt.Errorf("transaction failed with code %d: %s", sdkResp.Code, sdkResp.RawLog)
// 	// }

// 	// return sdkResp, nil
// 	return nil, nil
// }

// func prepareFactory(clientCtx client.Context, txf tx.Factory) (tx.Factory, error) {
// 	from := clientCtx.GetFromAddress()

// 	if err := txf.AccountRetriever().EnsureExists(clientCtx, from); err != nil {
// 		return txf, err
// 	}

// 	initNum, initSeq := txf.AccountNumber(), txf.Sequence()
// 	if initNum == 0 || initSeq == 0 {
// 		num, seq, err := txf.AccountRetriever().GetAccountNumberSequence(clientCtx, from)
// 		if err != nil {
// 			return txf, err
// 		}

// 		if initNum == 0 {
// 			txf = txf.WithAccountNumber(num)
// 		}

// 		if initSeq == 0 {
// 			txf = txf.WithSequence(seq)
// 		}
// 	}

// 	return txf, nil
// }
