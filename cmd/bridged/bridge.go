package main

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"

	// "log"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethstats"
	"github.com/ethereum/go-ethereum/les"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discv5"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nat"
	"github.com/ethereum/go-ethereum/params"
)

const (
	// RinkebyNetworkID is the network ID for the rinkeby network
	RinkebyNetworkID = 4
	// ContractAddressString is the hex string address of the contract to monitor
	ContractAddressString = "0x2bB346A12d83ed7573334Ed80348B476cb6de635"
)

var (
	// RinkebyGenesisBlock is the genesis block used by the Rinkeby test network
	RinkebyGenesisBlock = core.DefaultRinkebyGenesisBlock()
	// ContractAddress is the address of the contract to monitor
	ContractAddress = common.HexToAddress(ContractAddressString)
	// OneToken is the exact value of one token
	OneToken = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

var (
	ether = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

// ethBridge represents a prototype for a bridge between tft and erc20, able to call
// contract methods and listen for contract events
type ethBridge struct {
	config   *params.ChainConfig // Chain configurations for signing
	stack    *node.Node          // Ethereum protocol stack
	client   *ethclient.Client   // Client connection to the Ethereum chain
	keystore *keystore.KeyStore  // Keystore containing the signing info
	account  accounts.Account    // Account funding the bridge requests
	head     *types.Header       // Current head header of the bridge
	balance  *big.Int            // The current balance of the bridge (note: ethers only!)
	nonce    uint64              // Current pending nonce of the bridge
	price    *big.Int            // Current gas price to issue funds with
	lock     sync.RWMutex        // Lock protecting the bridge's internals
}

func newRinkebyEthBridge(port int, accountJSON, accountPass string, ethLog int) (*ethBridge, error) {
	log.Root().SetHandler(log.LvlFilterHandler(log.Lvl(ethLog), log.StreamHandler(os.Stderr, log.TerminalFormat(true))))
	var enodes []*discv5.Node
	for _, boot := range strings.Split(strings.Join(params.RinkebyBootnodes, ","), ",") {
		if url, err := discv5.ParseNode(boot); err == nil {
			enodes = append(enodes, url)
		} else {
			return nil, errors.New("Failed to parse bootnode URL" + "url" + boot + "err" + err.Error())
		}
	}
	ks := keystore.NewKeyStore(filepath.Join(os.Getenv("HOME"), ".bridge-proto", "keys"), keystore.StandardScryptN, keystore.StandardScryptP)
	var acc accounts.Account
	if accountJSON != "" {
		log.Info("Importing account")
		blob, err := ioutil.ReadFile(accountJSON)
		if err != nil {
			return nil, err
		}
		// Import the account
		acc, err = ks.Import(blob, accountPass, accountPass)
		if err != nil {
			return nil, err
		}

	} else {
		log.Info("Loading existing account")
		if len(ks.Accounts()) == 0 {
			return nil, errors.New("Failed to find an account in the keystore")
		}
		acc = ks.Accounts()[0]
	}
	log.Info("Unlocking account")
	if err := ks.Unlock(acc, accountPass); err != nil {
		return nil, err
	}
	log.Info("Account unlocked")
	return newEthBridge(RinkebyGenesisBlock, port, enodes, RinkebyNetworkID, "", ks)
}

func newEthBridge(genesis *core.Genesis, port int, enodes []*discv5.Node, network uint64, stats string, ks *keystore.KeyStore) (*ethBridge, error) {
	// Assemble the raw devp2p protocol stack
	stack, err := node.New(&node.Config{
		Name:    "tfb", //threefold bridge
		Version: params.VersionWithMeta,
		DataDir: filepath.Join(os.Getenv("HOME"), ".bridge-proto"),
		P2P: p2p.Config{
			NAT:              nat.Any(),
			NoDiscovery:      true,
			DiscoveryV5:      true,
			ListenAddr:       fmt.Sprintf(":%d", port),
			MaxPeers:         25,
			BootstrapNodesV5: enodes,
		},
	})
	if err != nil {
		return nil, err
	}
	// Assemble the Ethereum light client protocol
	if err := stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
		cfg := eth.DefaultConfig
		cfg.Ethash.DatasetDir = filepath.Join(os.Getenv("HOME"), ".bridge-proto", "ethash")
		cfg.SyncMode = downloader.LightSync
		cfg.NetworkId = network
		cfg.Genesis = genesis
		return les.New(ctx, &cfg)
	}); err != nil {
		return nil, err
	}
	// Assemble the ethstats monitoring and reporting service'
	if stats != "" {
		if err := stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
			var serv *les.LightEthereum
			ctx.Service(&serv)
			return ethstats.New(stats, nil, serv)
		}); err != nil {
			return nil, err
		}
	}
	// Boot up the client and ensure it connects to bootnodes
	if err := stack.Start(); err != nil {
		return nil, err
	}
	for _, boot := range enodes {
		old, err := enode.ParseV4(boot.String())
		if err != nil {
			stack.Server().AddPeer(old)
		}
	}

	// Attach to the client and retrieve and interesting metadatas
	api, err := stack.Attach()
	if err != nil {
		stack.Stop()
		return nil, err
	}
	client := ethclient.NewClient(api)
	return &ethBridge{
		config:   genesis.Config,
		stack:    stack,
		client:   client,
		keystore: ks,
		account:  ks.Accounts()[0],
	}, nil
}

// close terminates the Ethereum connection and tears down the bridge proto.
func (op *ethBridge) close() error {
	return op.stack.Stop()
}

// refresh attempts to retrieve the latest header from the chain and extract the
// associated bridge balance and nonce for connectivity caching.
func (op *ethBridge) refresh(head *types.Header) error {
	// Ensure a state update does not run for too long
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// If no header was specified, use the current chain head
	var err error
	if head == nil {
		if head, err = op.client.HeaderByNumber(ctx, nil); err != nil {
			return err
		}
	}
	// Retrieve the balance, nonce and gas price from the current head
	var (
		nonce   uint64
		price   *big.Int
		balance *big.Int
	)
	if price, err = op.client.SuggestGasPrice(ctx); err != nil {
		return err
	}
	if balance, err = op.client.BalanceAt(ctx, op.account.Address, head.Number); err != nil {
		return err
	}
	// Everything succeeded, update the cached stats
	op.lock.Lock()
	op.head, op.balance = head, balance
	op.price, op.nonce = price, nonce
	op.lock.Unlock()
	return nil
}

func (op *ethBridge) loop() {
	log.Info("Subscribing to eth headers")
	// channel to receive head updates from client on
	heads := make(chan *types.Header, 16)
	// subscribe to head upates
	sub, err := op.client.SubscribeNewHead(context.Background(), heads)
	if err != nil {
		// log.Fatal("Failed to subscribe to head events", "err", err)
	}
	defer sub.Unsubscribe()
	// channel so we can update the internal state from the heads
	update := make(chan *types.Header)
	go func() {
		for head := range update {
			// old heads should be ignored during a chain sync after some downtime
			if err := op.refresh(head); err != nil {
				log.Warn("Failed to update state", "block", head.Number, "err", err)
			}
			log.Info("Internal stats updated", "block", head.Number, "account balance", op.balance, "gas price", op.price, "nonce", op.nonce)
		}
	}()
	for head := range heads {
		select {
		// only process new head if another isn't being processed yet
		case update <- head:
		default:
			log.Info("Ignoring current head, update already in progress")
		}
	}
}

// SubscribeTransfers subscribes to new Transfer events on the given contract. This call blocks
// and prints out info about any transfer as it happened
func (op *ethBridge) SubscribeTransfers(contractAddress common.Address) error {
	filter, err := NewTTFT20Filterer(contractAddress, op.client)
	if err != nil {
		return err
	}
	sink := make(chan *TTFT20Transfer)
	opts := &bind.WatchOpts{Context: context.Background(), Start: nil}
	sub, err := filter.WatchTransfer(opts, sink, nil, nil)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()
	for {
		select {
		case err = <-sub.Err():
			return err
		case transfer := <-sink:
			log.Info("Noticed transfer event", "from", transfer.From, "to", transfer.To, "amount", transfer.Tokens)
		}
	}
}

// SubscribeTransfers subscribes to new Transfer events on the given contract. This call blocks
// and prints out info about any transfer as it happened
func (op *ethBridge) SubscribeMint(contractAddress common.Address) error {
	filter, err := NewTTFT20Filterer(contractAddress, op.client)
	if err != nil {
		return err
	}
	sink := make(chan *TTFT20Mint)
	opts := &bind.WatchOpts{Context: context.Background(), Start: nil}
	sub, err := filter.WatchMint(opts, sink, nil)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()
	for {
		select {
		case err = <-sub.Err():
			return err
		case mint := <-sink:
			log.Info("Noticed mint event", "receiver", mint.Receiver, "amount", mint.Tokens, "TFT tx id", mint.Txid)
		}
	}
}

//
func (op *ethBridge) TransferFunds(contractAddress common.Address, recipient common.Address, amount *big.Int) error {
	if amount == nil {
		return errors.New("invalid amount")
	}
	tr, err := NewTTFT20Transactor(contractAddress, op.client)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	opts := &bind.TransactOpts{Context: ctx, From: op.account.Address, Signer: op.GetSignerFunc(), Value: nil, Nonce: nil, GasLimit: 0, GasPrice: nil}
	_, err = tr.Transfer(opts, recipient, amount)
	if err != nil {
		return err
	}
	return nil
}

//
func (op *ethBridge) Mint(contractAddress common.Address, receiver common.Address, amount *big.Int, txID string) error {
	log.Info("Calling mint function in contract")
	if amount == nil {
		return errors.New("invalid amount")
	}
	tr, err := NewTTFT20Transactor(contractAddress, op.client)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	opts := &bind.TransactOpts{Context: ctx, From: op.account.Address, Signer: op.GetSignerFunc(), Value: nil, Nonce: nil, GasLimit: 0, GasPrice: nil}
	_, err = tr.MintTokens(opts, receiver, amount, txID)
	if err != nil {
		return err
	}
	return nil
}

func (op *ethBridge) GetSignerFunc() bind.SignerFn {
	return func(signer types.Signer, address common.Address, tx *types.Transaction) (*types.Transaction, error) {
		if address != op.account.Address {
			return nil, errors.New("not authorized to sign this account")
		}
		return op.keystore.SignTx(op.account, tx, big.NewInt(RinkebyNetworkID))
	}
}
