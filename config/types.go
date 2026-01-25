package config

import (
	"github.com/rs/zerolog"
	"github.com/spf13/viper"
)

type Seed struct {
	SeedPhrase     string `json:"seed_phrase"`
	DerivationPath string `json:"derivation_path"`
}

// required for the mapstructure tag
type ChainConfig struct {
	ChainId        string  `yaml:"chain_id" mapstructure:"chain_id"`
	Bech32Prefix   string  `yaml:"bech32_prefix" mapstructure:"bech32_prefix"`
	KeyringBackend string  `yaml:"keyring_backend" mapstructure:"keyring_backend"`
	RPCAddr        string  `yaml:"rpc_addr" mapstructure:"rpc_addr"`
	GRPCAddr       string  `yaml:"grpc_addr" mapstructure:"grpc_addr"`
	GasPrice       string  `yaml:"gas_price" mapstructure:"gas_price"`
	GasAdjustment  float64 `yaml:"gas_adjustment" mapstructure:"gas_adjustment"`
}

type Config struct {
	HomeDir      string      `yaml:"home_dir" mapstructure:"home_dir"`
	ProviderName string      `yaml:"provider_name" mapstructure:"provider_name"`
	ChainCfg     ChainConfig `yaml:"chain_config" mapstructure:"chain_config"`

	Ip            string    `yaml:"domain" mapstructure:"domain"`
	TotalSpace    int64     `yaml:"total_bytes_offered" mapstructure:"total_bytes_offered"`
	DataDirectory string    `yaml:"data_directory" mapstructure:"data_directory"`
	APICfg        APIConfig `yaml:"api_config" mapstructure:"api_config"`
}

// WalletInfo holds wallet information
type WalletInfo struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	CreatedAt string `json:"created_at"`
}

func DefaultIP() string {
	return "localhost"
}

func DefaultTotalSpace() int64 {
	return 1073741824
}

func DefaultDataDirectory() string {
	return "$HOME/.cygnus/data"
}

type StrayManagerConfig struct {
	CheckInterval   int64 `yaml:"check_interval" mapstructure:"check_interval"`
	RefreshInterval int64 `yaml:"refresh_interval" mapstructure:"refresh_interval"`
	HandCount       int   `yaml:"hands" mapstructure:"hands"`
}

// DefaultStrayManagerConfig returns the default configuration for the stray manager, setting check and refresh intervals and the hand count.
func DefaultStrayManagerConfig() StrayManagerConfig {
	return StrayManagerConfig{
		CheckInterval:   30,
		RefreshInterval: 120,
		HandCount:       2,
	}
}

type APIConfig struct {
	Port          int64 `yaml:"port" mapstructure:"port"`
	OpenGateway   bool  `yaml:"open_gateway" mapstructure:"open_gateway"`
	MaxUploadSize int64 `yaml:"max_upload_size" mapstructure:"max_upload_size"`
}

// DefaultAPIConfig returns the default APIConfig with preset ports, IPFS domain, search enabled, and an open gateway.
func DefaultAPIConfig() APIConfig {
	return APIConfig{
		Port:          3333,
		OpenGateway:   true,
		MaxUploadSize: 34359738368,
	}
}

func DefaultChainConfig() ChainConfig {
	return ChainConfig{
		ChainId:        "nebulix",
		RPCAddr:        "http://localhost:26657",
		GRPCAddr:       "localhost:9090",
		GasPrice:       "0.02uatl",
		GasAdjustment:  1.5,
		Bech32Prefix:   "atl",
		KeyringBackend: "test",
	}
}

func DefaultConfig() *Config {
	return &Config{
		ChainCfg:      DefaultChainConfig(),
		Ip:            DefaultIP(),
		TotalSpace:    DefaultTotalSpace(), // 1 gib default
		DataDirectory: DefaultDataDirectory(),

		APICfg: DefaultAPIConfig(),
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
		Int64("APIPort", c.APICfg.Port)
}

func init() {
	viper.SetDefault("StrayManagerCfg", DefaultStrayManagerConfig())
	viper.SetDefault("ChainCfg", DefaultChainConfig())
	viper.SetDefault("Ip", DefaultIP())
	viper.SetDefault("TotalSpace", DefaultTotalSpace()) // 1 gib defaul
	viper.SetDefault("DataDirectory", DefaultDataDirectory())
	viper.SetDefault("APICfg", DefaultAPIConfig())
}
