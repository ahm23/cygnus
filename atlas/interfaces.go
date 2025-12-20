package atlas

import (
	"github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	vestingtypes "github.com/cosmos/cosmos-sdk/x/auth/vesting/types"
)

func registerAccountInterfaces(interfaceRegistry types.InterfaceRegistry) {
	interfaceRegistry.RegisterImplementations(
		(*sdk.AccountI)(nil),
		&authtypes.BaseAccount{},
		&vestingtypes.BaseVestingAccount{},
		&vestingtypes.ContinuousVestingAccount{},
		&vestingtypes.DelayedVestingAccount{},
		&vestingtypes.PeriodicVestingAccount{},
		&vestingtypes.PermanentLockedAccount{},
	)
}
