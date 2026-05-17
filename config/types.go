package config

import "github.com/rs/zerolog"

type Seed struct {
	SeedPhrase     string `json:"seed_phrase"`
	DerivationPath string `json:"derivation_path"`
}

type ChainConfig struct {
	ChainId        string  `yaml:"chain_id" mapstructure:"chain_id"`
	KeyringBackend string  `yaml:"keyring_backend" mapstructure:"keyring_backend"`
	RPCAddr        string  `yaml:"rpc_addr" mapstructure:"rpc_addr"`
	GRPCAddr       string  `yaml:"grpc_addr" mapstructure:"grpc_addr"`
	GasPrice       string  `yaml:"gas_price" mapstructure:"gas_price"`
	GasAdjustment  float64 `yaml:"gas_adjustment" mapstructure:"gas_adjustment"`
}

type APIConfig struct {
	Port             int64 `yaml:"port" mapstructure:"port"`
	MaxUploadSize    int64 `yaml:"max_upload_size" mapstructure:"max_upload_size"`
	PrioritizeProofs bool  `yaml:"pause_uploads_for_proofs" mapstructure:"pause_uploads_for_proofs"`
	FsyncUploads     bool  `yaml:"fsync_uploads" mapstructure:"fsync_uploads"`
}

type Config struct {
	HomeDirectory string      `yaml:"home_dir" mapstructure:"home_dir"`
	DataDirectory string      `yaml:"data_directory" mapstructure:"data_directory"`
	ChainCfg      ChainConfig `yaml:"chain_config" mapstructure:"chain_config"`
	APICfg        APIConfig   `yaml:"api_config" mapstructure:"api_config"`

	ProviderName string `yaml:"provider_name" mapstructure:"provider_name"`
	Ip           string `yaml:"domain" mapstructure:"domain"`
	TotalSpace   int64  `yaml:"total_bytes_offered" mapstructure:"total_bytes_offered"`
}

// DefaultAPIConfig returns the default APIConfig with preset ports & advanced options focused on efficiency.
func DefaultAPIConfig() APIConfig {
	return APIConfig{
		Port:             3333,
		MaxUploadSize:    4 * 1024 * 1024 * 1024,
		FsyncUploads:     false,
		PrioritizeProofs: true,
	}
}

// DefaultChainConfig returns the default ChainConfig with preset RPC & GRPC endpoints.
func DefaultChainConfig() ChainConfig {
	return ChainConfig{
		ChainId:        "atlas-1",
		RPCAddr:        "https://rpc.atlasprotocol.cloud",
		GRPCAddr:       "https://grpc.atlasprotocol.cloud",
		GasPrice:       "0.025uatl",
		GasAdjustment:  1.5,
		KeyringBackend: "test",
	}
}

func DefaultConfig() *Config {
	return &Config{
		ProviderName:  "My First Provider",
		Ip:            "localhost",
		TotalSpace:    10 * 1024 * 1024 * 1024,
		DataDirectory: "$HOME/.cygnus/data",

		ChainCfg: DefaultChainConfig(),
		APICfg:   DefaultAPIConfig(),
	}
}

func (c Config) MarshalZerologObject(e *zerolog.Event) {
	e.Str("ChainRPCAddr", c.ChainCfg.RPCAddr).
		Str("ChainGRPCAddr", c.ChainCfg.GRPCAddr).
		Str("ChainGasPrice", c.ChainCfg.GasPrice).
		Float64("ChainGasAdjustment", c.ChainCfg.GasAdjustment).
		Str("IP", c.Ip).
		Int64("TotalSpace", c.TotalSpace).
		Str("DataDirectory", c.DataDirectory).
		Int64("API_Port", c.APICfg.Port).
		Bool("API_FsyncUploads", c.APICfg.FsyncUploads).
		Bool("API_PrioritizeProofs", c.APICfg.PrioritizeProofs)
}

// TODO: stray manager
// type StrayManagerConfig struct {
// 	CheckInterval   int64 `yaml:"check_interval" mapstructure:"check_interval"`
// 	RefreshInterval int64 `yaml:"refresh_interval" mapstructure:"refresh_interval"`
// 	HandCount       int   `yaml:"hands" mapstructure:"hands"`
// }

// // DefaultStrayManagerConfig returns the default configuration for the stray manager, setting check and refresh intervals and the hand count.
// func DefaultStrayManagerConfig() StrayManagerConfig {
// 	return StrayManagerConfig{
// 		CheckInterval:   30,
// 		RefreshInterval: 120,
// 		HandCount:       2,
// 	}
// }
