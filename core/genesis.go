// Copyright (C) 2019-2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm/stateconf"
	"github.com/ava-labs/libevm/log"
	ethparams "github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/trie"
	"github.com/ava-labs/libevm/triedb"
	"github.com/ava-labs/subnet-evm/core/state"
	"github.com/ava-labs/subnet-evm/params"
	"github.com/ava-labs/subnet-evm/plugin/evm/customrawdb"
	"github.com/ava-labs/subnet-evm/plugin/evm/upgrade/legacy"
	"github.com/ava-labs/subnet-evm/triedb/pathdb"
	"github.com/holiman/uint256"
)

//go:generate go run github.com/fjl/gencodec -type Genesis -field-override genesisSpecMarshaling -out gen_genesis.go

var errGenesisNoConfig = errors.New("genesis has no chain configuration")

// Deprecated: use types.Account instead.
type GenesisAccount = types.Account

// Deprecated: use types.GenesisAlloc instead.
type GenesisAlloc = types.GenesisAlloc

type Airdrop struct {
	// Address strings are hex-formatted common.Address
	Address common.Address `json:"address"`
}

// Genesis specifies the header fields, state of a genesis block. It also defines hard
// fork switch-over blocks through the chain configuration.
type Genesis struct {
	Config        *params.ChainConfig `json:"config"`
	Nonce         uint64              `json:"nonce"`
	Timestamp     uint64              `json:"timestamp"`
	ExtraData     []byte              `json:"extraData"`
	GasLimit      uint64              `json:"gasLimit"   gencodec:"required"`
	Difficulty    *big.Int            `json:"difficulty" gencodec:"required"`
	Mixhash       common.Hash         `json:"mixHash"`
	Coinbase      common.Address      `json:"coinbase"`
	Alloc         types.GenesisAlloc  `json:"alloc"      gencodec:"required"`
	AirdropHash   common.Hash         `json:"airdropHash"`
	AirdropAmount *big.Int            `json:"airdropAmount"`
	AirdropData   []byte              `json:"-"` // provided in a separate file, not serialized in this struct.

	// These fields are used for consensus tests. Please don't use them
	// in actual genesis blocks.
	Number        uint64      `json:"number"`
	GasUsed       uint64      `json:"gasUsed"`
	ParentHash    common.Hash `json:"parentHash"`
	BaseFee       *big.Int    `json:"baseFeePerGas"` // EIP-1559
	ExcessBlobGas *uint64     `json:"excessBlobGas"` // EIP-4844
	BlobGasUsed   *uint64     `json:"blobGasUsed"`   // EIP-4844
}

// field type overrides for gencodec
type genesisSpecMarshaling struct {
	Nonce         math.HexOrDecimal64
	Timestamp     math.HexOrDecimal64
	ExtraData     hexutil.Bytes
	GasLimit      math.HexOrDecimal64
	GasUsed       math.HexOrDecimal64
	Number        math.HexOrDecimal64
	Difficulty    *math.HexOrDecimal256
	Alloc         map[common.UnprefixedAddress]types.Account
	BaseFee       *math.HexOrDecimal256
	AirdropAmount *math.HexOrDecimal256
	ExcessBlobGas *math.HexOrDecimal64
	BlobGasUsed   *math.HexOrDecimal64
}

// GenesisMismatchError is raised when trying to overwrite an existing
// genesis block with an incompatible one.
type GenesisMismatchError struct {
	Stored, New common.Hash
}

func (e *GenesisMismatchError) Error() string {
	return fmt.Sprintf("database contains incompatible genesis (have %x, new %x)", e.Stored, e.New)
}

// SetupGenesisBlock writes or updates the genesis block in db.
// The block that will be used is:
//
//	                     genesis == nil       genesis != nil
//	                  +------------------------------------------
//	db has no genesis |  main-net default  |  genesis
//	db has genesis    |  from DB           |  genesis (if compatible)

// The argument [genesis] must be specified and must contain a valid chain config.
// If the genesis block has already been set up, then we verify the hash matches the genesis passed in
// and that the chain config contained in genesis is backwards compatible with what is stored in the database.
//
// The stored chain configuration will be updated if it is compatible (i.e. does not
// specify a fork block below the local head block). In case of a conflict, the
// error is a *params.ConfigCompatError and the new, unwritten config is returned.
func SetupGenesisBlock(
	db ethdb.Database, triedb *triedb.Database, genesis *Genesis, lastAcceptedHash common.Hash, skipChainConfigCheckCompatible bool,
) (*params.ChainConfig, common.Hash, error) {
	if genesis == nil {
		return nil, common.Hash{}, ErrNoGenesis
	}
	if genesis.Config == nil {
		return nil, common.Hash{}, errGenesisNoConfig
	}
	// Just commit the new block if there is no stored genesis block.
	stored := rawdb.ReadCanonicalHash(db, 0)
	if (stored == common.Hash{}) {
		log.Info("Writing genesis to database")
		block, err := genesis.Commit(db, triedb)
		if err != nil {
			return genesis.Config, common.Hash{}, err
		}
		return genesis.Config, block.Hash(), nil
	}
	// The genesis block is present(perhaps in ancient database) while the
	// state database is not initialized yet. It can happen that the node
	// is initialized with an external ancient store. Commit genesis state
	// in this case.
	header := rawdb.ReadHeader(db, stored, 0)
	if header.Root != types.EmptyRootHash && !triedb.Initialized(header.Root) {
		// Ensure the stored genesis matches with the given one.
		hash := genesis.ToBlock().Hash()
		if hash != stored {
			return genesis.Config, common.Hash{}, &GenesisMismatchError{stored, hash}
		}
		_, err := genesis.Commit(db, triedb)
		return genesis.Config, common.Hash{}, err
	}
	// Check whether the genesis block is already written.
	hash := genesis.ToBlock().Hash()
	if hash != stored {
		return genesis.Config, common.Hash{}, &GenesisMismatchError{stored, hash}
	}
	// Get the existing chain configuration.
	newcfg := genesis.Config
	if err := newcfg.CheckConfigForkOrder(); err != nil {
		return newcfg, common.Hash{}, err
	}
	storedcfg := customrawdb.ReadChainConfig(db, stored)
	// If there is no previously stored chain config, write the chain config to disk.
	if storedcfg == nil {
		// Note: this can happen since we did not previously write the genesis block and chain config in the same batch.
		log.Warn("Found genesis block without chain config")
		customrawdb.WriteChainConfig(db, stored, newcfg)
		return newcfg, stored, nil
	}

	// Notes on the following line:
	// - this is needed in coreth to handle the case where existing nodes do not
	//   have the Berlin or London forks initialized by block number on disk.
	//   See https://github.com/ava-labs/coreth/pull/667/files
	// - this is not needed in subnet-evm but it does not impact it either
	if err := params.SetEthUpgrades(storedcfg); err != nil {
		return genesis.Config, common.Hash{}, err
	}
	// Check config compatibility and write the config. Compatibility errors
	// are returned to the caller unless we're already at block zero.
	// we use last accepted block for cfg compatibility check. Note this allows
	// the node to continue if it previously halted due to attempting to process blocks with
	// an incorrect chain config.
	lastBlock := ReadBlockByHash(db, lastAcceptedHash)
	// this should never happen, but we check anyway
	// when we start syncing from scratch, the last accepted block
	// will be genesis block
	if lastBlock == nil {
		return newcfg, common.Hash{}, errors.New("missing last accepted block")
	}
	height := lastBlock.NumberU64()
	timestamp := lastBlock.Time()
	if skipChainConfigCheckCompatible {
		log.Info("skipping verifying activated network upgrades on chain config")
	} else {
		compatErr := storedcfg.CheckCompatible(newcfg, height, timestamp)
		if compatErr != nil && ((height != 0 && compatErr.RewindToBlock != 0) || (timestamp != 0 && compatErr.RewindToTime != 0)) {
			storedData, _ := params.ToWithUpgradesJSON(storedcfg).MarshalJSON()
			newData, _ := params.ToWithUpgradesJSON(newcfg).MarshalJSON()
			log.Error("found mismatch between config on database vs. new config", "storedConfig", string(storedData), "newConfig", string(newData), "err", compatErr)
			return newcfg, stored, compatErr
		}
	}
	// Required to write the chain config to disk to ensure both the chain config and upgrade bytes are persisted to disk.
	// Note: this intentionally removes an extra check from upstream.
	customrawdb.WriteChainConfig(db, stored, newcfg)
	return newcfg, stored, nil
}

// IsVerkle indicates whether the state is already stored in a verkle
// tree at genesis time.
func (g *Genesis) IsVerkle() bool {
	return g.Config.IsVerkle(new(big.Int).SetUint64(g.Number), g.Timestamp)
}

// ToBlock returns the genesis block according to genesis specification.
func (g *Genesis) ToBlock() *types.Block {
	db := rawdb.NewMemoryDatabase()
	return g.toBlock(db, triedb.NewDatabase(db, g.trieConfig()))
}

func (g *Genesis) trieConfig() *triedb.Config {
	if !g.IsVerkle() {
		return nil
	}
	return &triedb.Config{
		DBOverride: pathdb.Defaults.BackendConstructor,
		IsVerkle:   true,
	}
}

// TODO: migrate this function to "flush" for more similarity with upstream.
func (g *Genesis) toBlock(db ethdb.Database, triedb *triedb.Database) *types.Block {
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, triedb), nil)
	if err != nil {
		panic(err)
	}
	if g.AirdropHash != (common.Hash{}) {
		t := time.Now()
		h := common.BytesToHash(crypto.Keccak256(g.AirdropData))
		if g.AirdropHash != h {
			panic(fmt.Sprintf("expected standard allocation %s but got %s", g.AirdropHash, h))
		}
		airdrop := []*Airdrop{}
		if err := json.Unmarshal(g.AirdropData, &airdrop); err != nil {
			panic(err)
		}
		airdropAmount := uint256.MustFromBig(g.AirdropAmount)
		for _, alloc := range airdrop {
			statedb.SetBalance(alloc.Address, airdropAmount)
		}
		log.Debug(
			"applied airdrop allocation",
			"hash", h, "addrs", len(airdrop), "balance", g.AirdropAmount,
			"t", time.Since(t),
		)
	}

	head := &types.Header{
		Number:     new(big.Int).SetUint64(g.Number),
		Nonce:      types.EncodeNonce(g.Nonce),
		Time:       g.Timestamp,
		ParentHash: g.ParentHash,
		Extra:      g.ExtraData,
		GasLimit:   g.GasLimit,
		GasUsed:    g.GasUsed,
		BaseFee:    g.BaseFee,
		Difficulty: g.Difficulty,
		MixDigest:  g.Mixhash,
		Coinbase:   g.Coinbase,
	}

	// Configure any stateful precompiles that should be enabled in the genesis.
	blockContext := NewBlockContext(head.Number, head.Time)
	err = ApplyPrecompileActivations(g.Config, nil, blockContext, statedb)
	if err != nil {
		panic(fmt.Sprintf("unable to configure precompiles in genesis block: %v", err))
	}

	// Do custom allocation after airdrop in case an address shows up in standard
	// allocation
	for addr, account := range g.Alloc {
		statedb.SetBalance(addr, uint256.MustFromBig(account.Balance))
		statedb.SetCode(addr, account.Code)
		statedb.SetNonce(addr, account.Nonce)
		for key, value := range account.Storage {
			statedb.SetState(addr, key, value)
		}
	}
	root := statedb.IntermediateRoot(false)
	head.Root = root

	if g.GasLimit == 0 {
		head.GasLimit = ethparams.GenesisGasLimit
	}
	if g.Difficulty == nil {
		head.Difficulty = ethparams.GenesisDifficulty
	}
	if conf := g.Config; conf != nil {
		num := new(big.Int).SetUint64(g.Number)
		if params.GetExtra(conf).IsSubnetEVM(g.Timestamp) {
			if g.BaseFee != nil {
				head.BaseFee = g.BaseFee
			} else {
				head.BaseFee = new(big.Int).Set(params.GetExtra(g.Config).FeeConfig.MinBaseFee)
			}
		}
		if conf.IsCancun(num, g.Timestamp) {
			// EIP-4788: The parentBeaconBlockRoot of the genesis block is always
			// the zero hash. This is because the genesis block does not have a parent
			// by definition.
			head.ParentBeaconRoot = new(common.Hash)
			// EIP-4844 fields
			head.ExcessBlobGas = g.ExcessBlobGas
			head.BlobGasUsed = g.BlobGasUsed
			if head.ExcessBlobGas == nil {
				head.ExcessBlobGas = new(uint64)
			}
			if head.BlobGasUsed == nil {
				head.BlobGasUsed = new(uint64)
			}
		}
	}

	// Create the genesis block to use the block hash
	block := types.NewBlock(head, nil, nil, nil, trie.NewStackTrie(nil))
	triedbOpt := stateconf.WithTrieDBUpdatePayload(common.Hash{}, block.Hash())

	if _, err := statedb.Commit(0, false, stateconf.WithTrieDBUpdateOpts(triedbOpt)); err != nil {
		panic(fmt.Sprintf("unable to commit genesis block to statedb: %v", err))
	}
	// Commit newly generated states into disk if it's not empty.
	if root != types.EmptyRootHash {
		if err := triedb.Commit(root, true); err != nil {
			panic(fmt.Sprintf("unable to commit genesis block: %v", err))
		}
	}
	return block
}

// Commit writes the block and state of a genesis specification to the database.
// The block is committed as the canonical head block.
func (g *Genesis) Commit(db ethdb.Database, triedb *triedb.Database) (*types.Block, error) {
	block := g.toBlock(db, triedb)
	if block.Number().Sign() != 0 {
		return nil, errors.New("can't commit genesis block with number > 0")
	}
	config := g.Config
	if config == nil {
		return nil, errGenesisNoConfig
	}
	if err := config.CheckConfigForkOrder(); err != nil {
		return nil, err
	}
	batch := db.NewBatch()
	rawdb.WriteBlock(batch, block)
	rawdb.WriteReceipts(batch, block.Hash(), block.NumberU64(), nil)
	rawdb.WriteCanonicalHash(batch, block.Hash(), block.NumberU64())
	rawdb.WriteHeadBlockHash(batch, block.Hash())
	rawdb.WriteHeadHeaderHash(batch, block.Hash())
	customrawdb.WriteChainConfig(batch, block.Hash(), config)
	if err := batch.Write(); err != nil {
		return nil, fmt.Errorf("failed to write genesis block: %w", err)
	}
	return block, nil
}

// MustCommit writes the genesis block and state to db, panicking on error.
// The block is committed as the canonical head block.
func (g *Genesis) MustCommit(db ethdb.Database, triedb *triedb.Database) *types.Block {
	block, err := g.Commit(db, triedb)
	if err != nil {
		panic(err)
	}
	return block
}

func (g *Genesis) Verify() error {
	// Make sure genesis gas limit is consistent
	gasLimitConfig := params.GetExtra(g.Config).FeeConfig.GasLimit.Uint64()
	if gasLimitConfig != g.GasLimit {
		return fmt.Errorf(
			"gas limit in fee config (%d) does not match gas limit in header (%d)",
			gasLimitConfig,
			g.GasLimit,
		)
	}
	// Verify config
	if err := params.GetExtra(g.Config).Verify(); err != nil {
		return err
	}
	return nil
}

// GenesisBlockForTesting creates and writes a block in which addr has the given wei balance.
func GenesisBlockForTesting(db ethdb.Database, addr common.Address, balance *big.Int) *types.Block {
	g := Genesis{
		Config:  params.TestChainConfig,
		Alloc:   GenesisAlloc{addr: {Balance: balance}},
		BaseFee: big.NewInt(legacy.BaseFee),
	}
	return g.MustCommit(db, triedb.NewDatabase(db, triedb.HashDefaults))
}

// ReadBlockByHash reads the block with the given hash from the database.
func ReadBlockByHash(db ethdb.Reader, hash common.Hash) *types.Block {
	blockNumber := rawdb.ReadHeaderNumber(db, hash)
	if blockNumber == nil {
		return nil
	}
	return rawdb.ReadBlock(db, hash, *blockNumber)
}
