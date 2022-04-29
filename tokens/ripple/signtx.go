package ripple

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/anyswap/CrossChain-Router/v3/common"
	"github.com/anyswap/CrossChain-Router/v3/log"
	"github.com/anyswap/CrossChain-Router/v3/mpc"
	"github.com/anyswap/CrossChain-Router/v3/params"
	"github.com/anyswap/CrossChain-Router/v3/router"
	"github.com/anyswap/CrossChain-Router/v3/tokens"
	rcrypto "github.com/anyswap/CrossChain-Router/v3/tokens/ripple/rubblelabs/ripple/crypto"
	"github.com/anyswap/CrossChain-Router/v3/tokens/ripple/rubblelabs/ripple/data"
	"github.com/anyswap/CrossChain-Router/v3/tools/crypto"
	"github.com/btcsuite/btcd/btcec"
)

func (b *Bridge) verifyTransactionWithArgs(tx data.Transaction, args *tokens.BuildTxArgs) error {
	if tx.GetTransactionType() != data.PAYMENT {
		return fmt.Errorf("not a payment transaction")
	}

	payment, ok := tx.(*data.Payment)
	if !ok {
		return fmt.Errorf("type assertion error, transaction is not a payment")
	}

	to := payment.Destination.String()

	checkReceiver := args.Bind
	if !strings.EqualFold(to, checkReceiver) {
		return fmt.Errorf("[sign] verify tx receiver failed")
	}
	return nil
}

// MPCSignTransaction mpc sign raw tx
func (b *Bridge) MPCSignTransaction(rawTx interface{}, args *tokens.BuildTxArgs) (signedTx interface{}, txHash string, err error) {
	log.Debug("Ripple MPCSignTransaction")

	tx, ok := rawTx.(*data.Payment)
	if !ok {
		return nil, "", fmt.Errorf("type assertion error, transaction is not a payment")
	}

	err = b.verifyTransactionWithArgs(tx, args)
	if err != nil {
		log.Warn("Verify transaction failed", "error", err)
		return nil, "", err
	}

	if params.SignWithPrivateKey() {
		privKey := params.GetSignerPrivateKey(b.ChainConfig.ChainID)
		ecPrikey, errf := crypto.HexToECDSA(privKey)
		if errf != nil {
			return nil, "", errf
		}
		return b.SignTransactionWithPrivateKey(rawTx, ecPrikey)
	}

	jsondata, _ := json.Marshal(args.GetExtraArgs())
	msgContext := string(jsondata)
	msgHash, msg, err := data.SigningHash(tx)
	if err != nil {
		return nil, "", fmt.Errorf("get transaction signing hash failed: %w", err)
	}
	msg = append(tx.SigningPrefix().Bytes(), msg...)

	pubkeyStr := router.GetMPCPublicKey(args.From)
	pubkey := common.FromHex(pubkeyStr)
	isEd := isEd25519Pubkey(pubkey)

	var signContent string
	var signType string

	if isEd {
		// mpc ed public key has no 0xed prefix
		pubkeyStr = pubkeyStr[2:]
		// the real sign content is (signing prefix + msg)
		// when we hex encoding here, the mpc should do hex decoding there.
		signContent = common.ToHex(msg)
		signType = mpc.SignTypeEC256K1
	} else {
		signContent = msgHash.String()
		signType = mpc.SignTypeED25519
	}

	keyID, rsvs, err := mpc.DoSignOne(signType, pubkeyStr, signContent, msgContext)
	if err != nil {
		return nil, "", err
	}
	log.Info(b.ChainConfig.BlockChain+" MPCSignTransaction finished", "keyID", keyID, "signContent", signContent, "txid", args.SwapID)

	if len(rsvs) != 1 {
		return nil, "", fmt.Errorf("get sign status require one rsv but have %v (keyID = %v)", len(rsvs), keyID)
	}

	rsv := rsvs[0]
	log.Trace(b.ChainConfig.BlockChain+" MPCSignTransaction get rsv success", "keyID", keyID, "rsv", rsv)

	sig := rsvToSig(rsv, isEd)
	valid, err := rcrypto.Verify(pubkey, msgHash.Bytes(), msg, sig)
	if !valid || err != nil {
		return nil, "", fmt.Errorf("verify signature error (valid: %v): %v", valid, err)
	}

	signedTx, err = b.MakeSignedTransaction(pubkey, rsv, rawTx)
	if err != nil {
		return signedTx, "", err
	}

	txhash := signedTx.(data.Transaction).GetHash().String()

	return signedTx, txhash, nil
}

// SignTransactionWithPrivateKey sign tx with ECDSA private key
func (b *Bridge) SignTransactionWithPrivateKey(rawTx interface{}, privKey *ecdsa.PrivateKey) (signTx interface{}, txHash string, err error) {
	return b.SignTransactionWithRippleKey(rawTx, rcrypto.NewECDSAKeyFromPrivKeyBytes(privKey.D.Bytes()), nil)
}

// SignTransactionWithRippleKey sign tx with ripple key
func (b *Bridge) SignTransactionWithRippleKey(rawTx interface{}, key rcrypto.Key, keyseq *uint32) (signTx interface{}, txHash string, err error) {
	tx, ok := rawTx.(*data.Payment)
	if !ok {
		return nil, "", fmt.Errorf("sign transaction type assertion error")
	}

	msgHash, msg, err := data.SigningHash(tx)
	if err != nil {
		return nil, "", err
	}
	msg = append(tx.SigningPrefix().Bytes(), msg...)
	log.Info("Prepare to sign", "signing hash", msgHash.String(), "blob", fmt.Sprintf("%X", msg))

	sig, err := rcrypto.Sign(key.Private(keyseq), msgHash.Bytes(), msg)
	if err != nil {
		return nil, "", fmt.Errorf("sign hash error: %w", err)
	}

	pubkey := key.Public(keyseq)
	valid, err := rcrypto.Verify(pubkey, msgHash.Bytes(), msg, sig)
	if !valid || err != nil {
		return nil, "", fmt.Errorf("verify signature error (valid: %v): %v", valid, err)
	}

	var rsv string

	if isEd25519Pubkey(pubkey) {
		rsv = fmt.Sprintf("%X", sig)
	} else {
		signature, errf := btcec.ParseSignature(sig, btcec.S256())
		if errf != nil {
			return nil, "", fmt.Errorf("parse signature error: %w", errf)
		}
		rsv = fmt.Sprintf("%064X%064X00", signature.R, signature.S)
	}

	stx, err := b.MakeSignedTransaction(pubkey, rsv, tx)
	if err != nil {
		return nil, "", err
	}
	return stx, tx.Hash.String(), nil
}

// MakeSignedTransaction make signed transaction
func (b *Bridge) MakeSignedTransaction(pubkey []byte, rsv string, transaction interface{}) (signedTransaction interface{}, err error) {
	sig := rsvToSig(rsv, isEd25519Pubkey(pubkey))
	tx, ok := transaction.(*data.Payment)
	if !ok {
		return nil, fmt.Errorf("type assertion error, transaction is not a payment")
	}
	*tx.GetSignature() = data.VariableLength(sig)
	hash, _, err := data.Raw(tx)
	if err != nil {
		log.Warn("encode ripple tx error", "error", err)
		return nil, err
	}
	copy(tx.GetHash().Bytes(), hash.Bytes())
	return tx, nil
}

func isEd25519Pubkey(pubkey []byte) bool {
	return len(pubkey) == ed25519.PublicKeySize+1 && pubkey[0] == 0xED
}

func rsvToSig(rsv string, isEd bool) []byte {
	if isEd {
		return common.FromHex(rsv)
	}
	b, _ := hex.DecodeString(rsv)
	rx := hex.EncodeToString(b[:32])
	sx := hex.EncodeToString(b[32:64])
	r, _ := new(big.Int).SetString(rx, 16)
	s, _ := new(big.Int).SetString(sx, 16)
	signature := &btcec.Signature{
		R: r,
		S: s,
	}
	return signature.Serialize()
}
