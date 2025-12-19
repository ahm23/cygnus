package types

import (
	storagetypes "nebulix/x/storage/types"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

// QueryClients groups all query clients
type QueryClients struct {
	Auth    authtypes.QueryClient
	Bank    banktypes.QueryClient
	Storage storagetypes.QueryClient
}
