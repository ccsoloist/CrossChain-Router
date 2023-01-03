package cosmos

import (
	"math/big"
	"strings"
	"sync"

	"github.com/anyswap/CrossChain-Router/v3/log"
	"github.com/anyswap/CrossChain-Router/v3/tokens"
	cosmosClient "github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	codecTypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptoTypes "github.com/cosmos/cosmos-sdk/crypto/types"
	authTx "github.com/cosmos/cosmos-sdk/x/auth/tx"
)

var (
	supportedChainIDs     = make(map[string]bool)
	supportedChainIDsInit sync.Once
	ChainsList            = []string{"COSMOSHUB", "OSMOSIS", "COREUM", "SEI"}
)

const (
	mainnetNetWork = "mainnet"
	testnetNetWork = "testnet"
	devnetNetWork  = "devnet"
)

func BuildNewTxConfig() cosmosClient.TxConfig {
	interfaceRegistry := codecTypes.NewInterfaceRegistry()
	PublicKeyRegisterInterfaces(interfaceRegistry)
	protoCodec := codec.NewProtoCodec(interfaceRegistry)
	return authTx.NewTxConfig(protoCodec, authTx.DefaultSignModes)
}

func PublicKeyRegisterInterfaces(registry codecTypes.InterfaceRegistry) {
	registry.RegisterImplementations((*cryptoTypes.PubKey)(nil), &secp256k1.PubKey{})
}

// SupportsChainID supports chainID
func SupportsChainID(chainID *big.Int) bool {
	supportedChainIDsInit.Do(func() {
		for _, chainName := range ChainsList {
			supportedChainIDs[GetStubChainID(chainName, mainnetNetWork).String()] = true
			supportedChainIDs[GetStubChainID(chainName, testnetNetWork).String()] = true
			supportedChainIDs[GetStubChainID(chainName, devnetNetWork).String()] = true
		}
	})
	return supportedChainIDs[chainID.String()]
}

// IsSupportedCosmosSubChain is supported
func IsSupportedCosmosSubChain(chainName string) bool {
	var match bool
	chainName = strings.ToUpper(chainName)
	for _, chain := range ChainsList {
		if chain == chainName {
			match = true
			break
		}
	}
	return match
}

// GetStubChainID get stub chainID
func GetStubChainID(chainName, network string) *big.Int {
	chainName = strings.ToUpper(chainName)
	stubChainID := new(big.Int).SetBytes([]byte(chainName))
	switch network {
	case mainnetNetWork:
	case testnetNetWork:
		stubChainID.Add(stubChainID, big.NewInt(1))
	case devnetNetWork:
		stubChainID.Add(stubChainID, big.NewInt(2))
	default:
		log.Fatalf("unknown network %v", network)
	}
	stubChainID.Mod(stubChainID, tokens.StubChainIDBase)
	stubChainID.Add(stubChainID, tokens.StubChainIDBase)
	return stubChainID
}
