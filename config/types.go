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

	ProofInterval    uint64           `yaml:"proof_interval" mapstructure:"proof_interval"`
	Ip               string           `yaml:"domain" mapstructure:"domain"`
	TotalSpace       int64            `yaml:"total_bytes_offered" mapstructure:"total_bytes_offered"`
	DataDirectory    string           `yaml:"data_directory" mapstructure:"data_directory"`
	APICfg           APIConfig        `yaml:"api_config" mapstructure:"api_config"`
	BlockStoreConfig BlockStoreConfig `yaml:"block_store_config" mapstructure:"block_store_config"`
	QueueRateLimit   RateLimitConfig  `yaml:"queue_rate_limit" mapstructure:"queue_rate_limit"`
}

// WalletInfo holds wallet information
type WalletInfo struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	CreatedAt string `json:"created_at"`
}

func DefaultProofInterval() uint64 {
	return 700
}

func DefaultIP() string {
	return "https://example.com"
}

func DefaultTotalSpace() int64 {
	return 1092616192
}

func DefaultDataDirectory() string {
	return "$HOME/.cygnus/data"
}

type RateLimitConfig struct {
	PerTokenMs int64 `yaml:"per_token_ms" mapstructure:"per_token_ms"`
	Burst      int   `yaml:"burst" mapstructure:"burst"`
}

func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{PerTokenMs: 100, Burst: 30}
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
	Port        int64 `yaml:"port" mapstructure:"port"`
	OpenGateway bool  `yaml:"open_gateway" mapstructure:"open_gateway"`
}

// DefaultAPIConfig returns the default APIConfig with preset ports, IPFS domain, search enabled, and an open gateway.
func DefaultAPIConfig() APIConfig {
	return APIConfig{
		Port:        3333,
		OpenGateway: true,
	}
}

const (
	OptBadgerDS = "badgerds"
	OptFlatFS   = "flatfs"
)

type BlockStoreConfig struct {
	// *choosing badgerdb as block store will need to use the same directory
	// for data directory
	Directory string `yaml:"directory" mapstructure:"directory"`
	// data store options: flatfs, badgerdb
	Type string `yaml:"type" mapstructure:"type"`

	Key string `yaml:"key" mapstructure:"key"`
}

func DefaultBlockStoreConfig() BlockStoreConfig {
	return BlockStoreConfig{
		Directory: "$HOME/.cygnus/blockstore",
		Type:      OptFlatFS,
		Key:       "",
	}
}

// LegacyWallet handles keys from earlier versions of storage providers.
// v3 and earlier providers used private key to sign txs
// and by design it can't derive mnemonic seed which made
// it incompatible with cygnus's old wallet creation.
type LegacyWallet struct {
	Key     string `json:"key"`
	Address string `json:"address"`
}

func DefaultChainConfig() ChainConfig {
	return ChainConfig{
		RPCAddr:        "http://localhost:26657",
		GRPCAddr:       "localhost:9090",
		GasPrice:       "0.02unblx",
		GasAdjustment:  1.5,
		Bech32Prefix:   "nblx",
		KeyringBackend: "test",
	}
}

func DefaultConfig() *Config {
	return &Config{
		ProofInterval:    DefaultProofInterval(),
		ChainCfg:         DefaultChainConfig(),
		Ip:               DefaultIP(),
		TotalSpace:       DefaultTotalSpace(), // 1 gib default
		DataDirectory:    DefaultDataDirectory(),
		APICfg:           DefaultAPIConfig(),
		BlockStoreConfig: DefaultBlockStoreConfig(),
		QueueRateLimit:   DefaultRateLimitConfig(),
	}
}

func (c Config) MarshalZerologObject(e *zerolog.Event) {
	e.Uint64("ProofInterval", c.ProofInterval).
		Str("ChainRPCAddr", c.ChainCfg.RPCAddr).
		Str("ChainGRPCAddr", c.ChainCfg.GRPCAddr).
		Str("ChainGasPrice", c.ChainCfg.GasPrice).
		Float64("ChainGasAdjustment", c.ChainCfg.GasAdjustment).
		Str("IP", c.Ip).
		Int64("TotalSpace", c.TotalSpace).
		Str("DataDirectory", c.DataDirectory).
		Int64("APIPort", c.APICfg.Port).
		Str("BlockstoreBackend", c.BlockStoreConfig.Type).
		Int64("RateLimitPerTokenMs", c.QueueRateLimit.PerTokenMs).
		Int("RateLimitBurst", c.QueueRateLimit.Burst)
}

func init() {
	viper.SetDefault("ProofInterval", DefaultProofInterval())
	viper.SetDefault("StrayManagerCfg", DefaultStrayManagerConfig())
	viper.SetDefault("ChainCfg", DefaultChainConfig())
	viper.SetDefault("Ip", DefaultIP())
	viper.SetDefault("TotalSpace", DefaultTotalSpace()) // 1 gib defaul
	viper.SetDefault("DataDirectory", DefaultDataDirectory())
	viper.SetDefault("APICfg", DefaultAPIConfig())
	viper.SetDefault("BlockStoreConfig", DefaultBlockStoreConfig())
	viper.SetDefault("QueueRateLimit", DefaultRateLimitConfig())
}
