package scanner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/anyswap/CrossChain-Bridge/cmd/utils"
	"github.com/anyswap/CrossChain-Bridge/log"
	"github.com/anyswap/CrossChain-Bridge/tokens"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	//ethereum "github.com/fsn-dev/fsn-go-sdk/efsn"
	"github.com/gaozhengxin/bridgeAccounting/params"
	"github.com/gaozhengxin/bridgeAccounting/mongodb"
	//"github.com/gaozhengxin/bridgeAccounting/tools"
	"github.com/gaozhengxin/bridgeAccounting/accounting"
	"github.com/urfave/cli/v2"
)

var (
	scanReceiptFlag = &cli.BoolFlag{
		Name:  "scanReceipt",
		Usage: "scan transaction receipt instead of transaction",
	}

	startHeightFlag = &cli.Int64Flag{
		Name:  "start",
		Usage: "start height (start inclusive)",
		Value: -200,
	}

	timeoutFlag = &cli.Uint64Flag{
		Name:  "timeout",
		Usage: "timeout of scanning one block in seconds",
		Value: 300,
	}

	// StartCommand scan swaps on eth like blockchain, and do accounting
	StartCommand = &cli.Command{
		Action:    start,
		Name:      "start",
		Usage:     "scan cross chain swaps",
		ArgsUsage: " ",
		Description: `
scan cross chain swaps
`,
		Flags: []cli.Flag{
			utils.ConfigFileFlag,
		},
	}

	// 0. Deposit and 3. Redeemed
	transferFuncHash       = common.FromHex("0xa9059cbb")
	transferFromFuncHash   = common.FromHex("0x23b872dd")

	// 1. Mint
	swapinFuncHash = common.FromHex("0xec126c77")

	// 2. Burn
	addressSwapoutFuncHash = common.FromHex("0x628d6cba") // for ETH like `address` type address
	stringSwapoutFuncHash  = common.FromHex("0xad54056d") // for BTC like `string` type address

	// 0. Deposit and 3. Redeemed log, but also seen in 1. Mint
	transferLogTopic       = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

	// 1. Mint log
	swapinLogTopic = common.HexToHash("0x05d0634fe981be85c22e2942a880821b70095d84e152c3ea3c17a4e4250d9d61")

	// 2. Burn log
	addressSwapoutLogTopic = common.HexToHash("0x6b616089d04950dc06c45c6dd787d657980543f89651aec47924752c7d16c888")
	stringSwapoutLogTopic  = common.HexToHash("0x9c92ad817e5474d30a4378deface765150479363a897b0590fbb12ae9d89396b")
)

const (
	swapExistKeywords   = "mgoError: Item is duplicate"
	httpTimeoutKeywords = "Client.Timeout exceeded while awaiting headers"
)

type ethSwapScanner struct {
	gateway     string
	scanReceipt bool

	startHeightArgument int64

	endHeight    uint64
	stableHeight uint64
	jobCount     uint64

	processBlockTimeout time.Duration
	processBlockTimers  []*time.Timer

	client *ethclient.Client
	ctx    context.Context

	rpcInterval   time.Duration
	rpcRetryCount int

	chainId *big.Int
}

var (
	dbAPI mongodb.SyncAPI
/*
type SyncAPI interface {
	BaseQueryAPI
	SetStartHeight(srcStartHeight, dstStartHeight int64) error
	UpdateSyncedHeight(srcSyncedHeight, dstSyncedHeight int64) error
	AddDeposit(tokenCfg *param.TokenConfig, data SwapEvent) error
	AddMint(tokenCfg *param.TokenConfig, data SwapEvent) error
	AddBurn(tokenCfg *param.TokenConfig, data SwapEvent) error
	AddRedeemed(tokenCfg *param.TokenConfig, data SwapEvent) error
}
*/
)

func start(ctx *cli.Context) error {
	utils.SetLogger(ctx)
	cfg := params.LoadConfig(utils.GetConfigFilePath(ctx))
	go params.WatchAndReloadScanConfig()

	srcScanner := &ethSwapScanner{
		ctx:           context.Background(),
		rpcInterval:   1 * time.Second,
		rpcRetryCount: 3,
	}
	srcScanner.gateway = cfg.SrcGateway
	srcScanner.scanReceipt = cfg.SrcScanReceipt
	srcScanner.startHeightArgument = cfg.SrcStartHeightArgument
	srcScanner.endHeight = uint64(cfg.SrcEndHeight)
	srcScanner.stableHeight = uint64(cfg.SrcStableHeight)
	srcScanner.jobCount = uint64(cfg.SrcJobCount)
	srcScanner.processBlockTimeout = time.Duration(cfg.SrcProcessBlockTimeout) * time.Second

	log.Info("get src argument success",
		"gateway", srcScanner.gateway,
		"scanReceipt", srcScanner.scanReceipt,
		"start", srcScanner.startHeightArgument,
		"end", srcScanner.endHeight,
		"stable", srcScanner.stableHeight,
		"jobs", srcScanner.jobCount,
		"timeout", srcScanner.processBlockTimeout,
	)

	dstScanner := &ethSwapScanner{
		ctx:           context.Background(),
		rpcInterval:   1 * time.Second,
		rpcRetryCount: 3,
	}
	dstScanner.gateway = cfg.DstGateway
	dstScanner.scanReceipt = cfg.DstScanReceipt
	dstScanner.startHeightArgument = cfg.DstStartHeightArgument
	dstScanner.endHeight = uint64(cfg.DstEndHeight)
	dstScanner.stableHeight = uint64(cfg.DstStableHeight)
	dstScanner.jobCount = uint64(cfg.DstJobCount)
	dstScanner.processBlockTimeout = time.Duration(cfg.DstProcessBlockTimeout) * time.Second

	log.Info("get dst argument success",
		"gateway", dstScanner.gateway,
		"scanReceipt", dstScanner.scanReceipt,
		"start", dstScanner.startHeightArgument,
		"end", dstScanner.endHeight,
		"stable", dstScanner.stableHeight,
		"jobs", dstScanner.jobCount,
		"timeout", dstScanner.processBlockTimeout,
	)

	srcScanner.initClient()
	dstScanner.initClient()
	go srcScanner.run()
	go dstScanner.run()
	go accounting.StartAccounting()
	select {}
	return nil
}

func (scanner *ethSwapScanner) initClient() {
	ethcli, err := ethclient.Dial(scanner.gateway)
	if err != nil {
		log.Fatal("ethclient.Dail failed", "gateway", scanner.gateway, "err", err)
	}
	log.Info("ethclient.Dail gateway success", "gateway", scanner.gateway)
	scanner.client = ethcli
}

func (scanner *ethSwapScanner) run() {
	scanner.processBlockTimers = make([]*time.Timer, scanner.jobCount+1)
	for i := 0; i < len(scanner.processBlockTimers); i++ {
		scanner.processBlockTimers[i] = time.NewTimer(scanner.processBlockTimeout)
	}

	wend := scanner.endHeight
	if wend == 0 {
		wend = scanner.loopGetLatestBlockNumber()
	}
	if scanner.startHeightArgument != 0 {
		var start uint64
		if scanner.startHeightArgument > 0 {
			start = uint64(scanner.startHeightArgument)
		} else if scanner.startHeightArgument < 0 {
			start = wend - uint64(-scanner.startHeightArgument)
		}
		scanner.doScanRangeJob(start, wend)
	}
	if scanner.endHeight == 0 {
		scanner.scanLoop(wend)
	}
}

func (scanner *ethSwapScanner) doScanRangeJob(start, end uint64) {
	log.Info("start scan range job", "start", start, "end", end, "jobs", scanner.jobCount)
	if scanner.jobCount == 0 {
		log.Fatal("zero count jobs specified")
	}
	if start >= end {
		log.Fatalf("wrong scan range [%v, %v)", start, end)
	}
	jobs := scanner.jobCount
	count := end - start
	step := count / jobs
	if step == 0 {
		jobs = 1
		step = count
	}
	wg := new(sync.WaitGroup)
	for i := uint64(0); i < jobs; i++ {
		from := start + i*step
		to := start + (i+1)*step
		if i+1 == jobs {
			to = end
		}
		wg.Add(1)
		go scanner.scanRange(i+1, from, to, wg)
	}
	if scanner.endHeight != 0 {
		wg.Wait()
	}
}

func (scanner *ethSwapScanner) scanRange(job, from, to uint64, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Info(fmt.Sprintf("[%v] scan range", job), "from", from, "to", to)

	for h := from; h < to; h++ {
		scanner.scanBlock(job, h, false)
	}

	log.Info(fmt.Sprintf("[%v] scan range finish", job), "from", from, "to", to)
}

func (scanner *ethSwapScanner) scanLoop(from uint64) {
	stable := scanner.stableHeight
	log.Info("start scan loop job", "from", from, "stable", stable)
	for {
		latest := scanner.loopGetLatestBlockNumber()
		for h := from; h <= latest; h++ {
			scanner.scanBlock(0, h, true)
		}
		if from+stable < latest {
			from = latest - stable
		}
		time.Sleep(1 * time.Second)
	}
}

func (scanner *ethSwapScanner) loopGetLatestBlockNumber() uint64 {
	for { // retry until success
		header, err := scanner.client.HeaderByNumber(scanner.ctx, nil)
		if err == nil {
			log.Info("get latest block number success", "height", header.Number)
			return header.Number.Uint64()
		}
		log.Warn("get latest block number failed", "err", err)
		time.Sleep(scanner.rpcInterval)
	}
}

func (scanner *ethSwapScanner) loopGetTxReceipt(txHash common.Hash) (receipt *types.Receipt, err error) {
	for i := 0; i < 5; i++ { // with retry
		receipt, err = scanner.client.TransactionReceipt(scanner.ctx, txHash)
		if err == nil {
			return receipt, err
		}
		time.Sleep(scanner.rpcInterval)
	}
	return nil, err
}

func (scanner *ethSwapScanner) loopGetBlock(height uint64) (block *types.Block, err error) {
	blockNumber := new(big.Int).SetUint64(height)
	for i := 0; i < 5; i++ { // with retry
		block, err = scanner.client.BlockByNumber(scanner.ctx, blockNumber)
		if err == nil {
			return block, nil
		}
		log.Warn("get block failed", "height", height, "err", err)
		time.Sleep(scanner.rpcInterval)
	}
	return nil, err
}

func (scanner *ethSwapScanner) scanBlock(job, height uint64, cache bool) {
	block, err := scanner.loopGetBlock(height)
	if err != nil {
		return
	}
	blockHash := block.Hash().Hex()
	if cache && cachedBlocks.isScanned(blockHash) {
		return
	}
	log.Info(fmt.Sprintf("[%v] scan block %v", job, height), "hash", blockHash, "txs", len(block.Transactions()))

	scanner.processBlockTimers[job].Reset(scanner.processBlockTimeout)
SCANTXS:
	for i, tx := range block.Transactions() {
		select {
		case <-scanner.processBlockTimers[job].C:
			log.Warn(fmt.Sprintf("[%v] scan block %v timeout", job, height), "hash", blockHash, "txs", len(block.Transactions()))
			break SCANTXS
		default:
			log.Debug(fmt.Sprintf("[%v] scan tx in block %v index %v", job, height, i), "tx", tx.Hash().Hex())
			scanner.scanTransaction(tx)
		}
	}
	if cache {
		cachedBlocks.addBlock(blockHash)
	}
}

func (scanner *ethSwapScanner) scanTransaction(tx *types.Transaction) {
	if tx.To() == nil {
		return
	}
	txHash := tx.Hash().Hex()
	var receipt *types.Receipt
	if scanner.scanReceipt {
		r, err := scanner.loopGetTxReceipt(tx.Hash())
		if err != nil {
			log.Warn("get tx receipt error", "txHash", txHash, "err", err)
			return
		}
		receipt = r
	}

	for _, tokenCfg := range params.GetScanConfig().Tokens {
		swapTxType, swapEvent, verifyErr := scanner.verifyTransaction(tx, receipt, tokenCfg)
		if verifyErr != nil {
			log.Debug("verify tx failed", "txHash", txHash, "err", verifyErr)
		}

		mgoSwapEvent := convertToMgoSwapEvent(swapEvent, scanner.cachedDecimal(tokenCfg))

		var syncError error
		switch swapTxType {
		case TypeDeposit:
			syncError = dbAPI.AddDeposit(tokenCfg, mgoSwapEvent)
		case TypeMint:
			syncError = dbAPI.AddMint(tokenCfg, mgoSwapEvent)
		case TypeBurn:
			syncError = dbAPI.AddBurn(tokenCfg, mgoSwapEvent)
		case TypeRedeemed:
			syncError = dbAPI.AddRedeemed(tokenCfg, mgoSwapEvent)
		default:
			if verifyErr != nil {
				scanner.printVerifyError(txHash, verifyErr)
			}
		}
		if syncError != nil {
			log.Warn("Add swap event error", "swapTxType", swapTxType, "syncError", syncError)
		}
	}
}

type SwapTxType int8

const (
	TypeDeposit  SwapTxType = iota
	TypeMint
	TypeBurn
	TypeRedeemed
)

const TypeNull SwapTxType = -1

type SwapEvent struct {
	TxHash common.Hash
	BlockTime int64
	BlockNumber *big.Int
	Amount *big.Int
	User common.Address
}

func (scanner *ethSwapScanner) verifyTransaction(tx *types.Transaction, receipt *types.Receipt, tokenCfg *params.TokenConfig) (txType SwapTxType, swapData *SwapEvent, verifyErr error) {
	txTo := tx.To().Hex()
	txmsg, err := tx.AsMessage(types.NewEIP155Signer(scanner.chainId), nil)
	if err != nil {
		return TypeNull, nil, verifyErr
	}
	txFrom := txmsg.From()
	cmpTxTo := tokenCfg.TokenAddress
	depositAddress := tokenCfg.DepositAddress

	if tokenCfg.CallByContract != "" {
		cmpTxTo = tokenCfg.CallByContract
		if receipt == nil {
			txHash := tx.Hash()
			r, err := scanner.loopGetTxReceipt(txHash)
			if err != nil {
				log.Warn("get tx receipt error", "txHash", txHash.Hex(), "err", err)
				return TypeNull, nil, nil
			}
			receipt = r
		}
	}

	switch {
	case tokenCfg.IsSrcToken:
		// Src chain, Deposit or Redeemed
		if tokenCfg.IsNativeToken() {
			matched := strings.EqualFold(txTo, depositAddress)
			if matched {
				// deposit native
				swapData = &SwapEvent{
					TxHash: tx.Hash(),
					BlockTime: scanner.getBlockTimestamp(receipt.BlockNumber),
					BlockNumber: receipt.BlockNumber,
					Amount: tx.Value(),
					User: txFrom,
				}
				return TypeDeposit, swapData, nil
			} else if strings.EqualFold(txFrom.Hex(), depositAddress) {
				// redeemed native
				swapData = &SwapEvent{
					TxHash: tx.Hash(),
					BlockTime: scanner.getBlockTimestamp(receipt.BlockNumber),
					BlockNumber: receipt.BlockNumber,
					Amount: tx.Value(),
					User: *tx.To(),
				}
				return TypeRedeemed, swapData, nil
			}
			return TypeNull, nil, nil
		} else if strings.EqualFold(txTo, cmpTxTo) {
			if strings.EqualFold(txFrom.Hex(), depositAddress) == false {
				swapData, verifyErr = scanner.verifyErc20SwapinTx(tx, receipt, tokenCfg)
				if verifyErr == tokens.ErrTxWithWrongReceiver {
					return TypeNull, nil, verifyErr
				}
				// deposit erc20
				return TypeDeposit, swapData, verifyErr
			} else {
				swapData, verifyErr = scanner.verifyErc20RedeemTx(tx, receipt, tokenCfg)
				// erc20 redeemed
				return TypeRedeemed, swapData, verifyErr
			}
		}
	default:
		// Dst chain, Mint or Burn
		if strings.EqualFold(txTo, cmpTxTo) {
			if strings.EqualFold(txFrom.Hex(), depositAddress) {
				// Mint
				swapData, verifyErr = scanner.verifyMintTx(tx, receipt, tokenCfg)
				return TypeMint, swapData, verifyErr
			} else {
				// Burn
				swapData, verifyErr = scanner.verifySwapoutTx(tx, receipt, tokenCfg)
				if verifyErr == tokens.ErrTxWithWrongReceiver {
					return TypeNull, nil, verifyErr
				}
				return TypeBurn, swapData, verifyErr
			}
			return TypeNull, nil, verifyErr
		}
	}
	return TypeNull, nil, verifyErr
}

func (scanner *ethSwapScanner) printVerifyError(txHash string, verifyErr error) {
	switch {
	case errors.Is(verifyErr, tokens.ErrTxFuncHashMismatch):
	case errors.Is(verifyErr, tokens.ErrTxWithWrongReceiver):
	case errors.Is(verifyErr, tokens.ErrTxWithWrongContract):
	case errors.Is(verifyErr, tokens.ErrTxNotFound):
	default:
		log.Debug("verify swap error", "txHash", txHash, "err", verifyErr)
	}
}

// verify erc20 deposit
func (scanner *ethSwapScanner) verifyErc20SwapinTx(tx *types.Transaction, receipt *types.Receipt, tokenCfg *params.TokenConfig) (swapData *SwapEvent, err error) {
	swapData = &SwapEvent{
		TxHash: tx.Hash(),
		BlockTime: scanner.getBlockTimestamp(receipt.BlockNumber),
		BlockNumber: receipt.BlockNumber,
		Amount: nil,
		User: common.Address{},
	}
	if receipt == nil {
		err = scanner.parseErc20SwapinTxInput(tx.Data(), tokenCfg.DepositAddress, swapData)
	} else {
		err = scanner.parseErc20SwapinTxLogs(receipt.Logs, tokenCfg, swapData)
	}
	return swapData, err
}

// verify erc20 redeemed
func (scanner *ethSwapScanner) verifyErc20RedeemTx(tx *types.Transaction, receipt *types.Receipt, tokenCfg *params.TokenConfig) (swapData *SwapEvent, err error) {
	swapData = &SwapEvent{
		TxHash: tx.Hash(),
		BlockTime: scanner.getBlockTimestamp(receipt.BlockNumber),
		BlockNumber: receipt.BlockNumber,
		Amount: nil,
		User: common.Address{},
	}
	if receipt == nil {
		err = scanner.parseErc20RedeemTxInput(tx.Data(), tokenCfg.DepositAddress, swapData)
	} else {
		err = scanner.parseErc20RedeemTxLogs(receipt.Logs, tokenCfg, swapData)
	}
	return swapData, err
}

// verify burn
func (scanner *ethSwapScanner) verifySwapoutTx(tx *types.Transaction, receipt *types.Receipt, tokenCfg *params.TokenConfig) (swapData *SwapEvent, err error) {
	swapData = &SwapEvent{
		TxHash: tx.Hash(),
		BlockTime: scanner.getBlockTimestamp(receipt.BlockNumber),
		BlockNumber: receipt.BlockNumber,
		Amount: nil,
		User: common.Address{},
	}
	if receipt == nil {
		err = scanner.parseSwapoutTxInput(tx.Data(), swapData)
	} else {
		err = scanner.parseSwapoutTxLogs(receipt.Logs, tokenCfg, swapData)
	}
	return swapData, err
}

// verify mint
func (scanner *ethSwapScanner) verifyMintTx(tx *types.Transaction, receipt *types.Receipt, tokenCfg *params.TokenConfig) (swapData *SwapEvent, err error) {
	swapData = &SwapEvent{
		TxHash: tx.Hash(),
		BlockTime: scanner.getBlockTimestamp(receipt.BlockNumber),
		BlockNumber: receipt.BlockNumber,
		Amount: nil,
		User: common.Address{},
	}
	if receipt == nil {
		err = scanner.parseMintTxInput(tx.Data(), swapData)
	} else {
		err = scanner.parseMintTxLogs(receipt.Logs, tokenCfg, swapData)
	}
	return swapData, err
}

func (scanner *ethSwapScanner) parseErc20SwapinTxInput(input []byte, depositAddress string, swapData *SwapEvent) error {
	if len(input) < 4 {
		return tokens.ErrTxWithWrongInput
	}
	var receiver string
	funcHash := input[:4]
	switch {
	case bytes.Equal(funcHash, transferFuncHash):
		receiver = common.BytesToAddress(GetData(input, 4, 32)).Hex()
	case bytes.Equal(funcHash, transferFromFuncHash):
		receiver = common.BytesToAddress(GetData(input, 36, 32)).Hex()
	default:
		return tokens.ErrTxFuncHashMismatch
	}
	if !strings.EqualFold(receiver, depositAddress) {
		return tokens.ErrTxWithWrongReceiver
	}
	return nil
}

func (scanner *ethSwapScanner) parseErc20SwapinTxLogs(logs []*types.Log, tokenCfg *params.TokenConfig, swapData *SwapEvent) (err error) {
	targetContract := tokenCfg.TokenAddress
	depositAddress := tokenCfg.DepositAddress
	cmpLogTopic := transferLogTopic

	for _, rlog := range logs {
		if rlog.Removed {
			continue
		}
		if !strings.EqualFold(rlog.Address.Hex(), targetContract) {
			continue
		}
		if len(rlog.Topics) != 3 || rlog.Data == nil {
			continue
		}
		if rlog.Topics[0] == cmpLogTopic {
			receiver := common.BytesToAddress(rlog.Topics[2][:]).Hex()
			if strings.EqualFold(receiver, depositAddress) {
				return nil
			}
			return tokens.ErrTxWithWrongReceiver
		}
	}
	return tokens.ErrDepositLogNotFound
}

func (scanner *ethSwapScanner) parseErc20RedeemTxInput(input []byte, depositAddress string, swapData *SwapEvent) error {
	if len(input) < 4 {
		return tokens.ErrTxWithWrongInput
	}
	var receiver string
	funcHash := input[:4]
	switch {
	case bytes.Equal(funcHash, transferFuncHash):
		receiver = common.BytesToAddress(GetData(input, 4, 32)).Hex()
	case bytes.Equal(funcHash, transferFromFuncHash):
		receiver = common.BytesToAddress(GetData(input, 36, 32)).Hex()
	default:
		return tokens.ErrTxFuncHashMismatch
	}
	swapData.User = common.HexToAddress(receiver)
	return nil
}

func (scanner *ethSwapScanner) parseErc20RedeemTxLogs(logs []*types.Log, tokenCfg *params.TokenConfig, swapData *SwapEvent) (err error) {
	targetContract := tokenCfg.TokenAddress

	for _, rlog := range logs {
		if rlog.Removed {
			continue
		}
		if !strings.EqualFold(rlog.Address.Hex(), targetContract) {
			continue
		}
		if len(rlog.Topics) != 3 || rlog.Data == nil {
			continue
		}
		if rlog.Topics[0] == transferLogTopic {
			receiver := common.BytesToAddress(rlog.Topics[2][:]).Hex()
			swapData.User = common.HexToAddress(receiver)
		}
	}
	return tokens.ErrDepositLogNotFound
}

func (scanner *ethSwapScanner) parseSwapoutTxInput(input []byte, swapData *SwapEvent) error {
	if len(input) < 4 {
		return tokens.ErrTxWithWrongInput
	}
	funcHash := input[:4]
	if bytes.Equal(funcHash, addressSwapoutFuncHash) || bytes.Equal(funcHash, stringSwapoutFuncHash) {
		return nil
	}
	return tokens.ErrTxFuncHashMismatch
}

func (scanner *ethSwapScanner) parseSwapoutTxLogs(logs []*types.Log, tokenCfg *params.TokenConfig, swapData *SwapEvent) (err error) {
	targetContract := tokenCfg.TokenAddress

	for _, rlog := range logs {
		if rlog.Removed {
			continue
		}
		if !strings.EqualFold(rlog.Address.Hex(), targetContract) {
			continue
		}
		if len(rlog.Topics) != 2 || rlog.Data == nil {
			continue
		}
		if rlog.Topics[0] == addressSwapoutLogTopic || rlog.Topics[0] == stringSwapoutLogTopic {
			return nil
		}
	}
	return tokens.ErrSwapoutLogNotFound
}

func (scanner *ethSwapScanner) parseMintTxInput(input []byte, swapData *SwapEvent) error {
	if len(input) < 4 {
		return tokens.ErrTxWithWrongInput
	}
	funcHash := input[:4]
	if bytes.Equal(funcHash, swapinFuncHash) {
		return nil
	}
	return tokens.ErrTxFuncHashMismatch
}

func (scanner *ethSwapScanner) parseMintTxLogs(logs []*types.Log, tokenCfg *params.TokenConfig, swapData *SwapEvent) (err error) {
	targetContract := tokenCfg.TokenAddress

	for _, rlog := range logs {
		if rlog.Removed {
			continue
		}
		if !strings.EqualFold(rlog.Address.Hex(), targetContract) {
			continue
		}
		if len(rlog.Topics) != 2 || rlog.Data == nil {
			continue
		}
		if rlog.Topics[0] == swapinLogTopic {
			return nil
		}
	}
	return tokens.ErrSwapoutLogNotFound
}

type cachedSacnnedBlocks struct {
	capacity  int
	nextIndex int
	hashes    []string
}

var cachedBlocks = &cachedSacnnedBlocks{
	capacity:  100,
	nextIndex: 0,
	hashes:    make([]string, 100),
}

func (cache *cachedSacnnedBlocks) addBlock(blockHash string) {
	cache.hashes[cache.nextIndex] = blockHash
	cache.nextIndex = (cache.nextIndex + 1) % cache.capacity
}

func (cache *cachedSacnnedBlocks) isScanned(blockHash string) bool {
	for _, b := range cache.hashes {
		if b == blockHash {
			return true
		}
	}
	return false
}

func (scanner *ethSwapScanner) getBlockTimestamp(blockNumber *big.Int) int64 {
	header, err := scanner.client.HeaderByNumber(context.Background(), blockNumber)
	if err != nil {
		return 0
	}
	return int64(header.Time)
}

func GetData(data []byte, start uint64, size uint64) []byte {
	length := uint64(len(data))
	if start > length {
		start = length
	}
	end := start + size
	if end > length {
		end = length
	}
	return common.RightPadBytes(data[start:end], int(size))
}