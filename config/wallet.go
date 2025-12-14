package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nebulix/app"

	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/cosmos/go-bip39"
	"golang.org/x/term"
)

// InitWallet initializes a wallet
func InitWallet(homeDir string) (*WalletInfo, error) {
	// Load config first
	cfg, err := Init(homeDir)
	if err != nil {
		return nil, err
	}

	// Create keyring using YOUR blockchain's encoding
	kr, err := createKeyring(cfg.HomeDir, cfg.ChainCfg.KeyringBackend)
	if err != nil {
		return nil, fmt.Errorf("failed to create keyring: %w", err)
	}

	// Check if wallet already exists
	_, err = kr.Key("cygnus")
	if err == nil {
		return loadExistingWallet(kr, "cygnus", homeDir)
	}

	// Wallet doesn't exist, create new one
	fmt.Println("No existing wallet found.")
	fmt.Println("Would you like to:")
	fmt.Println("1. Create a new wallet")
	fmt.Println("2. Import from mnemonic")
	fmt.Print("Choose option (1/2): ")

	var choice string
	fmt.Scanln(&choice)

	switch choice {
	case "1":
		return createNewWallet(kr, "cygnus", homeDir)
	case "2":
		return importWallet(kr, "cygnus", homeDir)
	default:
		return nil, fmt.Errorf("invalid choice")
	}
}

// createKeyring creates a keyring instance using your blockchain's encoding
func createKeyring(homeDir, backend string) (keyring.Keyring, error) {
	// Get encoding config from YOUR blockchain
	encodingConfig := app.MakeEncodingConfig()

	// Create keyring with YOUR blockchain's codec
	kr, err := keyring.New(
		sdk.KeyringServiceName(),
		backend,
		homeDir,
		os.Stdin,
		encodingConfig.Codec,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create keyring: %w", err)
	}

	return kr, nil
}

// generateMnemonic generates a new BIP39 mnemonic
func generateMnemonic() (string, error) {
	// Generate 256 bits of entropy (24 words)
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", fmt.Errorf("failed to generate entropy: %w", err)
	}

	// Generate mnemonic from entropy
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return "", fmt.Errorf("failed to generate mnemonic: %w", err)
	}

	return mnemonic, nil
}

// validateMnemonic validates a BIP39 mnemonic
func validateMnemonic(mnemonic string) error {
	if !bip39.IsMnemonicValid(mnemonic) {
		return fmt.Errorf("invalid mnemonic phrase")
	}
	return nil
}

// createNewWallet creates a new wallet
func createNewWallet(kr keyring.Keyring, name, homeDir string) (*WalletInfo, error) {
	// Get password
	password, err := getPassword("Enter password for new wallet: ")
	if err != nil {
		return nil, err
	}

	confirmPassword, err := getPassword("Confirm password: ")
	if err != nil {
		return nil, err
	}

	if password != confirmPassword {
		return nil, fmt.Errorf("passwords do not match")
	}

	// Generate mnemonic using bip39
	mnemonic, err := generateMnemonic()
	if err != nil {
		return nil, fmt.Errorf("failed to generate mnemonic: %w", err)
	}

	// Create account
	info, err := kr.NewAccount(name, mnemonic, password, sdk.FullFundraiserPath, hd.Secp256k1)
	if err != nil {
		return nil, fmt.Errorf("failed to create account: %w", err)
	}

	addr, err := info.GetAddress()
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}

	// Save wallet info
	walletInfo := &WalletInfo{
		Name:      name,
		Address:   addr.String(),
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	if err := saveWalletInfo(homeDir, walletInfo); err != nil {
		return nil, fmt.Errorf("failed to save wallet info: %w", err)
	}

	fmt.Printf("\n✓ Wallet created successfully!\n")
	fmt.Printf("  Name: %s\n", name)
	fmt.Printf("  Address: %s\n", addr.String())

	fmt.Println("\n⚠️  IMPORTANT: Save this mnemonic phrase in a secure location!")
	fmt.Println("   It cannot be recovered if lost.")
	fmt.Println("   " + mnemonic)
	fmt.Println("")

	return walletInfo, nil
}

// importWallet imports a wallet from mnemonic
func importWallet(kr keyring.Keyring, name, homeDir string) (*WalletInfo, error) {
	fmt.Println("Enter mnemonic phrase (24 words, separated by spaces):")

	reader := bufio.NewReader(os.Stdin)
	mnemonic, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read mnemonic: %w", err)
	}

	// Clean up mnemonic
	mnemonic = strings.TrimSpace(mnemonic)

	// Validate mnemonic
	if err := validateMnemonic(mnemonic); err != nil {
		return nil, fmt.Errorf("invalid mnemonic: %w", err)
	}

	// Get password
	password, err := getPassword("Enter password for wallet: ")
	if err != nil {
		return nil, err
	}

	// Recover account
	info, err := kr.NewAccount(name, mnemonic, password, sdk.FullFundraiserPath, hd.Secp256k1)
	if err != nil {
		return nil, fmt.Errorf("failed to recover wallet: %w", err)
	}

	addr, err := info.GetAddress()
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}

	// Save wallet info
	walletInfo := &WalletInfo{
		Name:      name,
		Address:   addr.String(),
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	if err := saveWalletInfo(homeDir, walletInfo); err != nil {
		return nil, fmt.Errorf("failed to save wallet info: %w", err)
	}

	fmt.Printf("\n✓ Wallet recovered successfully!\n")
	fmt.Printf("  Name: %s\n", name)
	fmt.Printf("  Address: %s\n", addr.String())

	return walletInfo, nil
}

// loadExistingWallet loads an existing wallet
func loadExistingWallet(kr keyring.Keyring, name, homeDir string) (*WalletInfo, error) {
	info, err := kr.Key(name)
	if err != nil {
		return nil, fmt.Errorf("failed to load wallet: %w", err)
	}

	addr, err := info.GetAddress()
	if err != nil {
		return nil, fmt.Errorf("failed to get address: %w", err)
	}

	// Try to load saved wallet info
	walletInfo, err := LoadWalletInfo(homeDir)
	if err != nil {
		// Create new wallet info if file doesn't exist
		walletInfo = &WalletInfo{
			Name:      name,
			Address:   addr.String(),
			CreatedAt: time.Now().Format(time.RFC3339),
		}
		saveWalletInfo(homeDir, walletInfo)
	}

	fmt.Printf("✓ Wallet '%s' already exists\n", name)
	fmt.Printf("  Address: %s\n", addr.String())

	return walletInfo, nil
}

// getPassword securely reads a password from stdin
func getPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("failed to read password: %w", err)
	}
	return string(password), nil
}

// saveWalletInfo saves wallet information to a JSON file
func saveWalletInfo(homeDir string, info *WalletInfo) error {
	walletPath := filepath.Join(homeDir, "wallet.json")
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal wallet info: %w", err)
	}

	return os.WriteFile(walletPath, data, 0600)
}

// LoadWalletInfo loads wallet information from file
func LoadWalletInfo(homeDir string) (*WalletInfo, error) {
	walletPath := filepath.Join(homeDir, "wallet.json")
	data, err := os.ReadFile(walletPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read wallet file: %w", err)
	}

	var info WalletInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal wallet info: %w", err)
	}

	return &info, nil
}

// GetKeyring returns a keyring instance for the given config
func GetKeyring(cfg *Config) (keyring.Keyring, error) {
	return createKeyring(cfg.HomeDir, cfg.ChainCfg.KeyringBackend)
}

// VerifyWalletPassword verifies a wallet's password
func VerifyWalletPassword(kr keyring.Keyring, name, password string) error {
	_, err := kr.Key(name)
	if err != nil {
		return fmt.Errorf("wallet not found: %w", err)
	}

	// Try to sign a test message to verify password
	testMsg := []byte("test")
	_, _, err = kr.Sign(name, testMsg, signing.SignMode_SIGN_MODE_DIRECT)
	if err != nil {
		return fmt.Errorf("invalid password: %w", err)
	}

	return nil
}
