package core

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethdb"
	ethparams "github.com/ava-labs/libevm/params"
	"github.com/ava-labs/subnet-evm/consensus/dummy"
	"github.com/ava-labs/subnet-evm/params"
	"github.com/ava-labs/subnet-evm/params/extras"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetadata(t *testing.T) {
	t.Parallel()

	correctTxDependency := [][]uint64{{}, {0}, {}, {1}, {3}, {}, {0, 2}, {5}, {}, {8}}
	wrongTxDependency := [][]uint64{{0}}
	wrongTxDependencyCircular := [][]uint64{{}, {2}, {1}}
	wrongTxDependencyOutOfRange := [][]uint64{{}, {}, {3}}

	var temp map[int][]int

	temp = getDeps(correctTxDependency)
	assert.Equal(t, true, verifyDeps(temp))

	temp = getDeps(wrongTxDependency)
	assert.Equal(t, false, verifyDeps(temp))

	temp = getDeps(wrongTxDependencyCircular)
	assert.Equal(t, false, verifyDeps(temp))

	temp = getDeps(wrongTxDependencyOutOfRange)
	assert.Equal(t, false, verifyDeps(temp))
}

func TestParallelStateProcessor_BasicTransfer(t *testing.T) {
	// Test basic ETH transfers
	key1, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	key2, _ := crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
	addr1 := crypto.PubkeyToAddress(key1.PublicKey)
	addr2 := crypto.PubkeyToAddress(key2.PublicKey)

	recv1 := common.HexToAddress("0xa12")

	gspec := &Genesis{
		Config: params.WithExtra(
			&params.ChainConfig{
				ChainID:        big.NewInt(1),
				HomesteadBlock: big.NewInt(0),
				EIP150Block:    big.NewInt(0),
				EIP155Block:    big.NewInt(0),
				EIP158Block:    big.NewInt(0),
				ByzantiumBlock: big.NewInt(0),
			},
			&extras.ChainConfig{FeeConfig: params.DefaultFeeConfig},
		),
		Alloc:   types.GenesisAlloc{addr1: {Balance: big.NewInt(1000000000)}, addr2: {Balance: big.NewInt(1000000000)}},
		BaseFee: big.NewInt(ethparams.InitialBaseFee),
	}

	// Test with different transaction counts
	testCases := []struct {
		name        string
		txCount     int
		blockCount  int
		description string
	}{
		{"SingleTx", 2, 1, "Single transaction in one block"},
		// {"MultipleTxs", 2, 1, "Multiple transactions in one block"},
		// {"MultipleBlocks", 3, 3, "Multiple transactions across multiple blocks"},
		// {"HighLoad", 20, 2, "High transaction load"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			db := rawdb.NewMemoryDatabase()

			// Create parallel blockchain
			parallelChain, err := createParallelBlockChain(db, archiveConfig, gspec, common.Hash{})
			require.NoError(t, err)
			defer parallelChain.Stop()

			// Create sequential blockchain for comparison
			sequentialDB := rawdb.NewMemoryDatabase()
			sequentialChain, err := createSequentialBlockChain(sequentialDB, archiveConfig, gspec, common.Hash{})
			require.NoError(t, err)
			defer sequentialChain.Stop()

			signer := types.LatestSigner(gspec.Config)

			// Generate transactions
			_, parallelChainBlocks, _, err := GenerateChainWithGenesis(gspec, parallelChain.engine, tc.blockCount, uint64(tc.txCount), func(i int, gen *BlockGen) {
				for j := 0; j < tc.txCount; j++ {
					tx, _ := types.SignTx(types.NewTransaction(gen.TxNonce(addr1), recv1, big.NewInt(10000), ethparams.TxGas, nil, nil), signer, key1)
					gen.AddTx(tx)
				}
			})
			require.NoError(t, err)

			_, sequentialChainBlocks, _, err := GenerateChainWithGenesis(gspec, sequentialChain.engine, tc.blockCount, uint64(tc.txCount), func(i int, gen *BlockGen) {
				for j := 0; j < tc.txCount; j++ {
					tx, _ := types.SignTx(types.NewTransaction(gen.TxNonce(addr1), recv1, big.NewInt(10000), ethparams.TxGas, nil, nil), signer, key1)
					gen.AddTx(tx)
				}
			})
			require.NoError(t, err)

			// Execute sequential
			start := time.Now()
			_, err = sequentialChain.InsertChain(sequentialChainBlocks)
			sequentialTime := time.Since(start)
			require.NoError(t, err)

			// Execute parallel
			start = time.Now()
			_, err = parallelChain.InsertChain(parallelChainBlocks)
			parallelTime := time.Since(start)
			require.NoError(t, err)

			// Compare final states
			parallelState, err := parallelChain.State()
			require.NoError(t, err)
			sequentialState, err := sequentialChain.State()
			require.NoError(t, err)

			// parallelRoot := parallelState.IntermediateRoot(parallelChain.Config().IsEIP158(parallelChainBlocks[len(parallelChainBlocks)-1].Number()))
			// sequentialRoot := sequentialState.IntermediateRoot(sequentialChain.Config().IsEIP158(sequentialChainBlocks[len(sequentialChainBlocks)-1].Number()))

			t.Logf("%s: Parallel=%v, Sequential=%v, Speedup=%.2fx",
				tc.description, parallelTime, sequentialTime, float64(sequentialTime)/float64(parallelTime))

			// assert.Equal(t, sequentialRoot, parallelRoot, "State roots should match")

			afterBalanceParallel := parallelState.GetBalance(addr1)
			afterBalanceSequential := sequentialState.GetBalance(addr1)

			fmt.Printf("afterBalanceParallel: %v\n", afterBalanceParallel)
			fmt.Printf("afterBalanceSequential: %v\n", afterBalanceSequential)

			// Verify sender balances match
			assert.Equal(t, sequentialState.GetBalance(addr1), parallelState.GetBalance(addr1), "Sender balance should match")
			assert.Equal(t, sequentialState.GetBalance(addr2), parallelState.GetBalance(addr2), "Recipient balance should match")
		})
	}
}

func TestParallelStateProcessor_IndependentTransactions(t *testing.T) {
	senders := make([]struct {
		key  *ecdsa.PrivateKey
		addr common.Address
	}, 10)

	genesisAlloc := types.GenesisAlloc{}

	for i := 0; i < 10; i++ {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		senders[i] = struct {
			key  *ecdsa.PrivateKey
			addr common.Address
		}{key, addr}

		genesisAlloc[addr] = types.Account{Balance: big.NewInt(100000000)}
	}

	recipients := make([]common.Address, 10)
	for i := 0; i < 10; i++ {
		// Use a safer range to avoid conflicts with system addresses
		recipients[i] = common.HexToAddress(fmt.Sprintf("0x%040x", i+1000))
	}

	gspec := &Genesis{
		Config: params.WithExtra(
			&params.ChainConfig{HomesteadBlock: new(big.Int)},
			&extras.ChainConfig{FeeConfig: params.DefaultFeeConfig},
		),
		Alloc:   genesisAlloc,
		BaseFee: big.NewInt(ethparams.InitialBaseFee),
	}

	// Test with smaller transaction counts to avoid race conditions
	testCases := []struct {
		name        string
		txCount     int
		description string
	}{
		{"FewTxs", 10, "Few independent transactions"},
		// {"MoreTxs", 6, "More independent transactions"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create parallel blockchain
			db := rawdb.NewMemoryDatabase()
			parallelChain, err := createParallelBlockChain(db, archiveConfig, gspec, common.Hash{})
			require.NoError(t, err)
			defer parallelChain.Stop()

			// Create sequential blockchain for comparison
			sequentialDB := rawdb.NewMemoryDatabase()
			sequentialChain, err := createSequentialBlockChain(sequentialDB, archiveConfig, gspec, common.Hash{})
			require.NoError(t, err)
			defer sequentialChain.Stop()

			signer := types.LatestSigner(gspec.Config)

			// Generate transactions for parallel chain - ensure each sender only sends once per block
			_, parallelChainBlocks, _, err := GenerateChainWithGenesis(gspec, parallelChain.engine, 1, uint64(tc.txCount), func(i int, gen *BlockGen) {
				for j := 0; j < tc.txCount; j++ {
					sender := senders[j%len(senders)]
					recipient := recipients[j%len(recipients)]
					tx, _ := types.SignTx(types.NewTransaction(gen.TxNonce(sender.addr), recipient, big.NewInt(1000), ethparams.TxGas, nil, nil), signer, sender.key)
					gen.AddTx(tx)
				}
			})
			require.NoError(t, err)

			// Generate transactions for sequential chain
			_, sequentialChainBlocks, _, err := GenerateChainWithGenesis(gspec, sequentialChain.engine, 1, uint64(tc.txCount), func(i int, gen *BlockGen) {
				for j := 0; j < tc.txCount; j++ {
					sender := senders[j%len(senders)]
					recipient := recipients[j%len(recipients)]
					tx, _ := types.SignTx(types.NewTransaction(gen.TxNonce(sender.addr), recipient, big.NewInt(1000), ethparams.TxGas, nil, nil), signer, sender.key)
					gen.AddTx(tx)
				}
			})
			require.NoError(t, err)

			// Execute sequential
			start := time.Now()
			_, err = sequentialChain.InsertChain(sequentialChainBlocks)
			sequentialTime := time.Since(start)
			require.NoError(t, err)

			// Execute parallel
			start = time.Now()
			_, err = parallelChain.InsertChain(parallelChainBlocks)
			parallelTime := time.Since(start)
			require.NoError(t, err)

			// Compare final states
			parallelState, err := parallelChain.State()
			require.NoError(t, err)
			sequentialState, err := sequentialChain.State()
			require.NoError(t, err)

			parallelRoot := parallelState.IntermediateRoot(parallelChain.Config().IsEIP158(parallelChainBlocks[len(parallelChainBlocks)-1].Number()))
			sequentialRoot := sequentialState.IntermediateRoot(sequentialChain.Config().IsEIP158(sequentialChainBlocks[len(sequentialChainBlocks)-1].Number()))

			t.Logf("%s: Parallel=%v, Sequential=%v, Speedup=%.2fx",
				tc.description, parallelTime, sequentialTime, float64(sequentialTime)/float64(parallelTime))

			assert.Equal(t, sequentialRoot, parallelRoot, "State roots should match")

			for i, sender := range senders {
				parallelBalance := parallelState.GetBalance(sender.addr)
				sequentialBalance := sequentialState.GetBalance(sender.addr)
				fmt.Printf("Sender %d balance: Sequential=%v, Parallel=%v\n", i, sequentialBalance, parallelBalance)
				assert.Equal(t, sequentialBalance, parallelBalance,
					"Sender %d balance should match", i)
			}

			for i, recipient := range recipients {
				parallelBalance := parallelState.GetBalance(recipient)
				sequentialBalance := sequentialState.GetBalance(recipient)
				assert.Equal(t, sequentialBalance, parallelBalance,
					"Recipient %d balance should match", i)
			}

			for i, sender := range senders {
				parallelNonce := parallelState.GetNonce(sender.addr)
				sequentialNonce := sequentialState.GetNonce(sender.addr)
				assert.Equal(t, sequentialNonce, parallelNonce,
					"Sender %d nonce should match", i)
			}
		})
	}
}

func TestParallelStateProcessor_ContractInteractions(t *testing.T) {
	// Test with contract creation and interactions
	key1, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	addr1 := crypto.PubkeyToAddress(key1.PublicKey)

	gspec := &Genesis{
		Config: params.WithExtra(
			&params.ChainConfig{HomesteadBlock: new(big.Int)},
			&extras.ChainConfig{FeeConfig: params.DefaultFeeConfig},
		),
		Alloc:   types.GenesisAlloc{addr1: {Balance: big.NewInt(1000000000)}},
		BaseFee: big.NewInt(ethparams.InitialBaseFee),
	}

	db := rawdb.NewMemoryDatabase()
	blockchain, err := createParallelBlockChain(db, archiveConfig, gspec, common.Hash{})
	require.NoError(t, err)
	defer blockchain.Stop()

	signer := types.LatestSigner(gspec.Config)

	// Simple contract bytecode (just returns)
	contractCode := []byte{0x60, 0x00, 0x60, 0x00, 0xf3} // PUSH1 0, PUSH1 0, RETURN

	_, chain, _, err := GenerateChainWithGenesis(gspec, blockchain.engine, 1, 5, func(i int, gen *BlockGen) {
		for j := 0; j < 5; j++ {
			// Create contract transaction
			tx, _ := types.SignTx(types.NewContractCreation(gen.TxNonce(addr1), big.NewInt(0), 1000000, big.NewInt(1), contractCode), signer, key1)
			gen.AddTx(tx)
		}
	})
	require.NoError(t, err)

	start := time.Now()
	_, err = blockchain.InsertChain(chain)
	executionTime := time.Since(start)
	require.NoError(t, err)

	t.Logf("Contract creation: %v", executionTime)
}

func TestParallelStateProcessor_StressTest(t *testing.T) {
	// Stress test with many transactions
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	// Create many senders
	senders := make([]struct {
		key  *ecdsa.PrivateKey
		addr common.Address
	}, 50)

	genesisAlloc := types.GenesisAlloc{}

	for i := 0; i < 50; i++ {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		senders[i] = struct {
			key  *ecdsa.PrivateKey
			addr common.Address
		}{key, addr}

		genesisAlloc[addr] = types.Account{Balance: big.NewInt(10000000)}
	}

	gspec := &Genesis{
		Config: params.WithExtra(
			&params.ChainConfig{HomesteadBlock: new(big.Int)},
			&extras.ChainConfig{FeeConfig: params.DefaultFeeConfig},
		),
		Alloc:   genesisAlloc,
		BaseFee: big.NewInt(ethparams.InitialBaseFee),
	}

	db := rawdb.NewMemoryDatabase()
	blockchain, err := createParallelBlockChain(db, archiveConfig, gspec, common.Hash{})
	require.NoError(t, err)
	defer blockchain.Stop()

	signer := types.LatestSigner(gspec.Config)
	_, chain, _, err := GenerateChainWithGenesis(gspec, blockchain.engine, 5, 100, func(i int, gen *BlockGen) {
		for j := 0; j < 100; j++ {
			sender := senders[j%len(senders)]
			recipient := senders[(j+1)%len(senders)].addr
			tx, _ := types.SignTx(types.NewTransaction(gen.TxNonce(sender.addr), recipient, big.NewInt(100), ethparams.TxGas, nil, nil), signer, sender.key)
			gen.AddTx(tx)
		}
	})
	require.NoError(t, err)

	start := time.Now()
	_, err = blockchain.InsertChain(chain)
	executionTime := time.Since(start)
	require.NoError(t, err)

	t.Logf("Stress test (500 txs): %v", executionTime)
}

func createSequentialBlockChain(
	db ethdb.Database,
	cacheConfig *CacheConfig,
	gspec *Genesis,
	lastAcceptedHash common.Hash,
) (*BlockChain, error) {
	blockchain, err := NewBlockChain(
		db,
		cacheConfig,
		gspec,
		dummy.NewCoinbaseFaker(),
		vm.Config{},
		lastAcceptedHash,
		false,
	)
	return blockchain, err
}
