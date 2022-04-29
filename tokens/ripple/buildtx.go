package ripple

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/anyswap/CrossChain-Router/v3/common"
	"github.com/anyswap/CrossChain-Router/v3/log"
	"github.com/anyswap/CrossChain-Router/v3/params"
	"github.com/anyswap/CrossChain-Router/v3/router"
	"github.com/anyswap/CrossChain-Router/v3/tokens"
	"github.com/anyswap/CrossChain-Router/v3/tokens/ripple/rubblelabs/ripple/crypto"
	"github.com/anyswap/CrossChain-Router/v3/tokens/ripple/rubblelabs/ripple/data"
)

var (
	defaultFee     int64 = 10
	accountReserve       = big.NewInt(10000000)
)

// BuildRawTransaction build raw tx
//nolint:gocyclo // ok
func (b *Bridge) BuildRawTransaction(args *tokens.BuildTxArgs) (rawTx interface{}, err error) {
	if !params.IsTestMode && args.ToChainID.String() != b.ChainConfig.ChainID {
		return nil, tokens.ErrToChainIDMismatch
	}
	if args.From == "" {
		return nil, fmt.Errorf("forbid empty sender")
	}
	routerMPC, err := router.GetRouterMPC(args.GetTokenID(), b.ChainConfig.ChainID)
	if err != nil {
		return nil, err
	}
	if !common.IsEqualIgnoreCase(args.From, routerMPC) {
		log.Error("build tx mpc mismatch", "have", args.From, "want", routerMPC)
		return nil, tokens.ErrSenderMismatch
	}

	switch args.SwapType {
	case tokens.ERC20SwapType:
	default:
		return nil, tokens.ErrSwapTypeNotSupported
	}

	mpcPubkey := router.GetMPCPublicKey(args.From)
	if mpcPubkey == "" {
		return nil, tokens.ErrMissMPCPublicKey
	}

	erc20SwapInfo := args.ERC20SwapInfo
	multichainToken := router.GetCachedMultichainToken(erc20SwapInfo.TokenID, args.ToChainID.String())
	if multichainToken == "" {
		log.Warn("get multichain token failed", "tokenID", erc20SwapInfo.TokenID, "chainID", args.ToChainID)
		return nil, tokens.ErrMissTokenConfig
	}

	token := b.GetTokenConfig(multichainToken)
	if token == nil {
		return nil, tokens.ErrMissTokenConfig
	}

	receiver, amount, err := b.getReceiverAndAmount(args, multichainToken)
	if err != nil {
		return nil, err
	}

	var (
		sequence uint64
		fee      string
		toTag    *uint32
	)

	extra := args.Extra
	if extra == nil {
		extra = b.swapoutDefaultArgs(args, multichainToken)
		args.Extra = extra
		sequence = *extra.Sequence
		fee = *extra.Fee
	} else {
		if extra.Sequence != nil {
			sequence = *extra.Sequence
		}
		if extra.Fee != nil {
			fee = *extra.Fee
		}
	}

	amt, err := getPaymentAmount(amount, token)
	if err != nil {
		return nil, err
	}

	if token.RippleExtra.IsNative() {
		needAmount := new(big.Int).Add(amount, b.getMinReserveFee())
		err = b.checkNativeBalance(args.From, needAmount, true)
		if err != nil {
			return nil, err
		}
		err = b.checkNativeBalance(receiver, amount, false)
		if err != nil {
			return nil, err
		}
	} else {
		err = b.checkNativeBalance(receiver, nil, false)
		if err != nil {
			return nil, err
		}
		err = b.checkNonNativeBalance(token.RippleExtra.Currency, token.RippleExtra.Issuer, args.From, amt)
		if err != nil {
			return nil, err
		}
	}

	ripplePubKey := ImportPublicKey(common.FromHex(mpcPubkey))
	rawtx, _, _ := NewUnsignedPaymentTransaction(ripplePubKey, nil, uint32(sequence), receiver, toTag, amt.String(), fee, "", "", false, false, false)

	return rawtx, err
}

func (b *Bridge) getReceiverAndAmount(args *tokens.BuildTxArgs, multichainToken string) (receiver string, amount *big.Int, err error) {
	erc20SwapInfo := args.ERC20SwapInfo
	receiver = args.Bind
	if receiver == "" || !b.IsValidAddress(args.Bind) {
		log.Warn("swapout to wrong receiver", "receiver", args.Bind)
		return receiver, amount, errors.New("can not swapout to empty or invalid receiver")
	}
	fromBridge := router.GetBridgeByChainID(args.FromChainID.String())
	if fromBridge == nil {
		return receiver, amount, tokens.ErrNoBridgeForChainID
	}
	fromTokenCfg := fromBridge.GetTokenConfig(erc20SwapInfo.Token)
	if fromTokenCfg == nil {
		log.Warn("get token config failed", "chainID", args.FromChainID, "token", erc20SwapInfo.Token)
		return receiver, amount, tokens.ErrMissTokenConfig
	}
	toTokenCfg := b.GetTokenConfig(multichainToken)
	if toTokenCfg == nil {
		return receiver, amount, tokens.ErrMissTokenConfig
	}
	amount = tokens.CalcSwapValue(erc20SwapInfo.TokenID, b.ChainConfig.ChainID, args.OriginValue, fromTokenCfg.Decimals, toTokenCfg.Decimals, args.OriginFrom, args.OriginTxTo)
	return receiver, amount, err
}

func getPaymentAmount(amount *big.Int, token *tokens.TokenConfig) (*data.Amount, error) {
	currency, exist := currencyMap[token.RippleExtra.Currency]
	if !exist {
		return nil, fmt.Errorf("non exist currency %v", token.RippleExtra.Currency)
	}

	if !amount.IsInt64() {
		return nil, fmt.Errorf("amount value %v is overflow of type int64", amount)
	}

	if currency.IsNative() { // native XRP
		return data.NewAmount(amount.Int64())
	}

	issuer, exist := issuerMap[token.RippleExtra.Issuer]
	if !exist {
		return nil, fmt.Errorf("non exist issuer %v", token.RippleExtra.Issuer)
	}

	// get a Value of amount*10^(-decimals)
	value, err := data.NewNonNativeValue(amount.Int64(), -int64(token.Decimals))
	if err != nil {
		log.Error("getPaymentAmount failed", "currency", token.RippleExtra.Currency, "issuer", token.RippleExtra.Issuer, "amount", amount, "decimals", token.Decimals, "err", err)
		return nil, err
	}

	return &data.Amount{
		Value:    value,
		Currency: currency,
		Issuer:   *issuer,
	}, nil
}

func (b *Bridge) getMinReserveFee() *big.Int {
	config := params.GetRouterConfig()
	if config == nil {
		return big.NewInt(0)
	}
	minReserve := params.GetMinReserveFee(b.ChainConfig.ChainID)
	if minReserve == nil {
		minReserve = big.NewInt(100000) // default 0.1 XRP
	}
	return minReserve
}

func (b *Bridge) swapoutDefaultArgs(txargs *tokens.BuildTxArgs, multichainToken string) *tokens.AllExtras {
	args := &tokens.AllExtras{
		Sequence: new(uint64),
		Fee:      new(string),
	}

	token := b.GetTokenConfig(multichainToken)
	if token == nil {
		return args
	}

	seq, err := b.GetSeq(txargs)
	if err != nil {
		log.Warn("Get sequence error when setting default ripple args", "error", err)
	}
	*args.Sequence = *seq

	fee := defaultFee
	fei, _ := data.NewNativeValue(fee)
	*args.Fee = fei.String()

	return args
}

func (b *Bridge) checkNativeBalance(account string, amount *big.Int, isPay bool) error {
	balance, err := b.GetBalance(account)
	if err != nil && balance == nil {
		balance = big.NewInt(0)
	}

	remain := balance
	if amount != nil {
		if isPay {
			remain = new(big.Int).Sub(balance, amount)
		} else {
			remain = new(big.Int).Add(balance, amount)
		}
	}

	if remain.Cmp(accountReserve) < 0 {
		if isPay {
			return fmt.Errorf("insufficient native balance, sender: %v", account)
		}
		return fmt.Errorf("insufficient native balance, receiver: %v", account)
	}

	return nil
}

func (b *Bridge) checkNonNativeBalance(currency, issuer, account string, amount *data.Amount) error {
	accl, err := b.GetAccountLine(currency, issuer, account)
	if err != nil {
		return err
	}
	if accl.Balance.Value.Compare(*amount.Value) < 0 {
		return fmt.Errorf("insufficient %v balance, issuer: %v, account: %v", currency, issuer, account)
	}
	return nil
}

// GetTxBlockInfo impl NonceSetter interface
func (b *Bridge) GetTxBlockInfo(txHash string) (blockHeight, blockTime uint64) {
	txStatus, err := b.GetTransactionStatus(txHash)
	if err != nil {
		return 0, 0
	}
	return txStatus.BlockHeight, txStatus.BlockTime
}

// GetPoolNonce impl NonceSetter interface
func (b *Bridge) GetPoolNonce(address, _height string) (uint64, error) {
	var nonce uint32
	account, err := b.GetAccount(address)
	if err != nil {
		return 0, fmt.Errorf("cannot get account, %w", err)
	}
	if seq := account.AccountData.Sequence; seq != nil {
		nonce = *seq
	}
	return uint64(nonce), nil
}

// GetSeq returns account tx sequence
func (b *Bridge) GetSeq(args *tokens.BuildTxArgs) (nonceptr *uint64, err error) {
	nonceVal, err := b.GetPoolNonce(args.From, "")
	if err != nil {
		return nil, err
	}
	if args == nil {
		return &nonceVal, nil
	}
	nonceVal = b.AdjustNonce(args.From, nonceVal)
	return &nonceVal, nil
}

// NewUnsignedPaymentTransaction build ripple payment tx
// Partial and limit must be false
func NewUnsignedPaymentTransaction(
	key crypto.Key, keyseq *uint32, txseq uint32, dest string, destinationTag *uint32,
	amt string, fee string, memo, path string, nodirect, partial, limit bool,
) (data.Transaction, data.Hash256, []byte) {
	if partial {
		log.Warn("Building tx with partial")
	}
	if limit {
		log.Warn("Building tx with limit")
	}

	destination, amount := parseAccount(dest), parseAmount(amt)
	payment := &data.Payment{
		Destination:    *destination,
		Amount:         *amount,
		DestinationTag: destinationTag,
	}
	payment.TransactionType = data.PAYMENT

	if memo != "" {
		memoStr := new(data.Memo)
		memoStr.Memo.MemoType = []byte("BIND")
		memoStr.Memo.MemoData = []byte(memo)
		payment.Memos = append(payment.Memos, *memoStr)
	}

	if path != "" {
		payment.Paths = parsePaths(path)
	}
	payment.Flags = new(data.TransactionFlag)
	if nodirect {
		*payment.Flags |= data.TxNoDirectRipple
	}
	if partial {
		*payment.Flags |= data.TxPartialPayment
	}
	if limit {
		*payment.Flags |= data.TxLimitQuality
	}

	base := payment.GetBase()

	base.Sequence = txseq

	fei, err := data.NewValue(fee, true)
	if err != nil {
		return nil, data.Hash256{}, nil
	}
	base.Fee = *fei

	copy(base.Account[:], key.Id(keyseq))

	payment.InitialiseForSigning()
	copy(payment.GetPublicKey().Bytes(), key.Public(keyseq))
	hash, msg, err := data.SigningHash(payment)
	if err != nil {
		log.Warn("Generate ripple tx signing hash error", "error", err)
		return nil, data.Hash256{}, nil
	}
	log.Info("Build unsigned tx success", "signing hash", hash.String(), "blob", fmt.Sprintf("%X", msg))

	return payment, hash, msg
}
