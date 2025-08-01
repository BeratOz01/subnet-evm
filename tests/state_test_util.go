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
// Copyright 2015 The go-ethereum Authors
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

package tests

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethdb"
	ethparams "github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/libevm/triedb"
	"github.com/ava-labs/subnet-evm/consensus/misc/eip4844"
	"github.com/ava-labs/subnet-evm/core"
	"github.com/ava-labs/subnet-evm/core/state"
	"github.com/ava-labs/subnet-evm/core/state/snapshot"
	"github.com/ava-labs/subnet-evm/params"
	"github.com/ava-labs/subnet-evm/plugin/evm/customrawdb"
	"github.com/ava-labs/subnet-evm/triedb/firewood"
	"github.com/ava-labs/subnet-evm/triedb/hashdb"
	"github.com/ava-labs/subnet-evm/triedb/pathdb"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"
)

// StateTest checks transaction processing without block context.
// See https://github.com/ethereum/EIPs/issues/176 for the test format specification.
type StateTest struct {
	json stJSON
}

// StateSubtest selects a specific configuration of a General State Test.
type StateSubtest struct {
	Fork  string
	Index int
}

func (t *StateTest) UnmarshalJSON(in []byte) error {
	return json.Unmarshal(in, &t.json)
}

type stJSON struct {
	Env  stEnv                    `json:"env"`
	Pre  types.GenesisAlloc       `json:"pre"`
	Tx   stTransaction            `json:"transaction"`
	Out  hexutil.Bytes            `json:"out"`
	Post map[string][]stPostState `json:"post"`
}

type stPostState struct {
	Root            common.UnprefixedHash `json:"hash"`
	Logs            common.UnprefixedHash `json:"logs"`
	TxBytes         hexutil.Bytes         `json:"txbytes"`
	ExpectException string                `json:"expectException"`
	Indexes         struct {
		Data  int `json:"data"`
		Gas   int `json:"gas"`
		Value int `json:"value"`
	}
}

//go:generate go run github.com/fjl/gencodec -type stEnv -field-override stEnvMarshaling -out gen_stenv.go
type stEnv struct {
	Coinbase      common.Address `json:"currentCoinbase"   gencodec:"required"`
	Difficulty    *big.Int       `json:"currentDifficulty" gencodec:"optional"`
	Random        *big.Int       `json:"currentRandom"     gencodec:"optional"`
	GasLimit      uint64         `json:"currentGasLimit"   gencodec:"required"`
	Number        uint64         `json:"currentNumber"     gencodec:"required"`
	Timestamp     uint64         `json:"currentTimestamp"  gencodec:"required"`
	BaseFee       *big.Int       `json:"currentBaseFee"    gencodec:"optional"`
	ExcessBlobGas *uint64        `json:"currentExcessBlobGas" gencodec:"optional"`
}

type stEnvMarshaling struct {
	Coinbase      common.UnprefixedAddress
	Difficulty    *math.HexOrDecimal256
	Random        *math.HexOrDecimal256
	GasLimit      math.HexOrDecimal64
	Number        math.HexOrDecimal64
	Timestamp     math.HexOrDecimal64
	BaseFee       *math.HexOrDecimal256
	ExcessBlobGas *math.HexOrDecimal64
}

//go:generate go run github.com/fjl/gencodec -type stTransaction -field-override stTransactionMarshaling -out gen_sttransaction.go
type stTransaction struct {
	GasPrice             *big.Int            `json:"gasPrice"`
	MaxFeePerGas         *big.Int            `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *big.Int            `json:"maxPriorityFeePerGas"`
	Nonce                uint64              `json:"nonce"`
	To                   string              `json:"to"`
	Data                 []string            `json:"data"`
	AccessLists          []*types.AccessList `json:"accessLists,omitempty"`
	GasLimit             []uint64            `json:"gasLimit"`
	Value                []string            `json:"value"`
	PrivateKey           []byte              `json:"secretKey"`
	Sender               *common.Address     `json:"sender"`
	BlobVersionedHashes  []common.Hash       `json:"blobVersionedHashes,omitempty"`
	BlobGasFeeCap        *big.Int            `json:"maxFeePerBlobGas,omitempty"`
}

// nolint: unused
type stTransactionMarshaling struct {
	GasPrice             *math.HexOrDecimal256
	MaxFeePerGas         *math.HexOrDecimal256
	MaxPriorityFeePerGas *math.HexOrDecimal256
	Nonce                math.HexOrDecimal64
	GasLimit             []math.HexOrDecimal64
	PrivateKey           hexutil.Bytes
	BlobGasFeeCap        *math.HexOrDecimal256
}

// GetChainConfig takes a fork definition and returns a chain config.
// The fork definition can be
// - a plain forkname, e.g. `Byzantium`,
// - a fork basename, and a list of EIPs to enable; e.g. `Byzantium+1884+1283`.
func GetChainConfig(forkString string) (baseConfig *params.ChainConfig, eips []int, err error) {
	var (
		splitForks            = strings.Split(forkString, "+")
		ok                    bool
		baseName, eipsStrings = splitForks[0], splitForks[1:]
	)

	// NOTE: this is added to support mapping geth fork names to
	// subnet-evm fork names.
	forkAliases := map[string]string{
		"Berlin":       "Pre-SubnetEVM",
		"London":       "SubnetEVM",
		"ArrowGlacier": "SubnetEVM",
		"GrayGlacier":  "SubnetEVM",
		"Merge":        "SubnetEVM",
	}
	if alias, ok := forkAliases[baseName]; ok {
		baseName = alias
	}

	if baseConfig, ok = Forks[baseName]; !ok {
		return nil, nil, UnsupportedForkError{baseName}
	}
	for _, eip := range eipsStrings {
		if eipNum, err := strconv.Atoi(eip); err != nil {
			return nil, nil, fmt.Errorf("syntax error, invalid eip number %v", eipNum)
		} else {
			if !vm.ValidEip(eipNum) {
				return nil, nil, fmt.Errorf("syntax error, invalid eip number %v", eipNum)
			}
			eips = append(eips, eipNum)
		}
	}
	return baseConfig, eips, nil
}

// Subtests returns all valid subtests of the test.
func (t *StateTest) Subtests() []StateSubtest {
	var sub []StateSubtest
	for fork, pss := range t.json.Post {
		for i := range pss {
			sub = append(sub, StateSubtest{fork, i})
		}
	}
	return sub
}

// checkError checks if the error returned by the state transition matches any expected error.
// A failing expectation returns a wrapped version of the original error, if any,
// or a new error detailing the failing expectation.
// This function does not return or modify the original error, it only evaluates and returns expectations for the error.
func (t *StateTest) checkError(subtest StateSubtest, err error) error {
	expectedError := t.json.Post[subtest.Fork][subtest.Index].ExpectException
	if err == nil && expectedError == "" {
		return nil
	}
	if err == nil && expectedError != "" {
		return fmt.Errorf("expected error %q, got no error", expectedError)
	}
	if err != nil && expectedError == "" {
		return fmt.Errorf("unexpected error: %w", err)
	}
	if err != nil && expectedError != "" {
		// Ignore expected errors (TODO MariusVanDerWijden check error string)
		return nil
	}
	return nil
}

// Run executes a specific subtest and verifies the post-state and logs
func (t *StateTest) Run(subtest StateSubtest, vmconfig vm.Config, snapshotter bool, scheme string, postCheck func(err error, st *StateTestState)) (result error) {
	st, root, err := t.RunNoVerify(subtest, vmconfig, snapshotter, scheme)
	// Invoke the callback at the end of function for further analysis.
	defer func() {
		postCheck(result, &st)
		st.Close()
	}()

	checkedErr := t.checkError(subtest, err)
	if checkedErr != nil {
		return checkedErr
	}
	// The error has been checked; if it was unexpected, it's already returned.
	if err != nil {
		// Here, an error exists but it was expected.
		// We do not check the post state or logs.
		return nil
	}
	post := t.json.Post[subtest.Fork][subtest.Index]
	// N.B: We need to do this in a two-step process, because the first Commit takes care
	// of self-destructs, and we need to touch the coinbase _after_ it has potentially self-destructed.
	if root != common.Hash(post.Root) {
		return fmt.Errorf("post state root mismatch: got %x, want %x", root, post.Root)
	}
	if logs := rlpHash(st.StateDB.Logs()); logs != common.Hash(post.Logs) {
		return fmt.Errorf("post state logs hash mismatch: got %x, want %x", logs, post.Logs)
	}
	st.StateDB, _ = state.New(root, st.StateDB.Database(), st.Snapshots)
	return nil
}

// RunNoVerify runs a specific subtest and returns the statedb and post-state root.
// Remember to call state.Close after verifying the test result!
func (t *StateTest) RunNoVerify(subtest StateSubtest, vmconfig vm.Config, snapshotter bool, scheme string) (state StateTestState, root common.Hash, err error) {
	config, eips, err := GetChainConfig(subtest.Fork)
	if err != nil {
		return state, common.Hash{}, UnsupportedForkError{subtest.Fork}
	}
	vmconfig.ExtraEips = eips

	block := t.genesis(config).ToBlock()
	state = MakePreState(rawdb.NewMemoryDatabase(), t.json.Pre, snapshotter, scheme)

	var baseFee *big.Int
	if params.GetExtra(config).IsSubnetEVM(0) {
		baseFee = t.json.Env.BaseFee
		if baseFee == nil {
			// Retesteth uses `0x10` for genesis baseFee. Therefore, it defaults to
			// parent - 2 : 0xa as the basefee for 'this' context.
			baseFee = big.NewInt(0x0a)
		}
	}
	post := t.json.Post[subtest.Fork][subtest.Index]
	msg, err := t.json.Tx.toMessage(post, baseFee)
	if err != nil {
		return state, common.Hash{}, err
	}

	{ // Blob transactions may be present after the Cancun fork.
		// In production,
		// - the header is verified against the max in eip4844.go:VerifyEIP4844Header
		// - the block body is verified against the header in block_validator.go:ValidateBody
		// Here, we just do this shortcut smaller fix, since state tests do not
		// utilize those codepaths
		if len(msg.BlobHashes)*ethparams.BlobTxBlobGasPerBlob > ethparams.MaxBlobGasPerBlock {
			return state, common.Hash{}, errors.New("blob gas exceeds maximum")
		}
	}

	// Try to recover tx with current signer
	if len(post.TxBytes) != 0 {
		var ttx types.Transaction
		err := ttx.UnmarshalBinary(post.TxBytes)
		if err != nil {
			return state, common.Hash{}, err
		}
		if _, err := types.Sender(types.LatestSigner(config), &ttx); err != nil {
			return state, common.Hash{}, err
		}
	}

	// Prepare the EVM.
	txContext := core.NewEVMTxContext(msg)
	context := core.NewEVMBlockContext(block.Header(), nil, &t.json.Env.Coinbase)
	context.GetHash = vmTestBlockHash
	context.BaseFee = baseFee
	context.Random = nil
	if config.IsLondon(new(big.Int)) && t.json.Env.Random != nil {
		rnd := common.BigToHash(t.json.Env.Random)
		context.Random = &rnd
		context.Difficulty = big.NewInt(0)
	}
	if config.IsCancun(new(big.Int), block.Time()) && t.json.Env.ExcessBlobGas != nil {
		context.BlobBaseFee = eip4844.CalcBlobFee(*t.json.Env.ExcessBlobGas)
	}
	evm := vm.NewEVM(context, txContext, state.StateDB, config, vmconfig)

	// Execute the message.
	snapshot := state.StateDB.Snapshot()
	gaspool := new(core.GasPool)
	gaspool.AddGas(block.GasLimit())
	_, err = core.ApplyMessage(evm, msg, gaspool)
	if err != nil {
		state.StateDB.RevertToSnapshot(snapshot)
	}
	// Add 0-value mining reward. This only makes a difference in the cases
	// where
	// - the coinbase self-destructed, or
	// - there are only 'bad' transactions, which aren't executed. In those cases,
	//   the coinbase gets no txfee, so isn't created, and thus needs to be touched
	state.StateDB.AddBalance(block.Coinbase(), new(uint256.Int))

	// Commit state mutations into database.
	root, _ = state.StateDB.Commit(block.NumberU64(), config.IsEIP158(block.Number()))
	return state, root, err
}

func (t *StateTest) genesis(config *params.ChainConfig) *core.Genesis {
	genesis := &core.Genesis{
		Config:     config,
		Coinbase:   t.json.Env.Coinbase,
		Difficulty: t.json.Env.Difficulty,
		GasLimit:   t.json.Env.GasLimit,
		Number:     t.json.Env.Number,
		Timestamp:  t.json.Env.Timestamp,
		Alloc:      t.json.Pre,
	}
	if t.json.Env.Random != nil {
		// Post-Merge
		genesis.Mixhash = common.BigToHash(t.json.Env.Random)
		genesis.Difficulty = big.NewInt(0)
	}
	return genesis
}

func (tx *stTransaction) toMessage(ps stPostState, baseFee *big.Int) (*core.Message, error) {
	var from common.Address
	// If 'sender' field is present, use that
	if tx.Sender != nil {
		from = *tx.Sender
	} else if len(tx.PrivateKey) > 0 {
		// Derive sender from private key if needed.
		key, err := crypto.ToECDSA(tx.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("invalid private key: %v", err)
		}
		from = crypto.PubkeyToAddress(key.PublicKey)
	}
	// Parse recipient if present.
	var to *common.Address
	if tx.To != "" {
		to = new(common.Address)
		if err := to.UnmarshalText([]byte(tx.To)); err != nil {
			return nil, fmt.Errorf("invalid to address: %v", err)
		}
	}

	// Get values specific to this post state.
	if ps.Indexes.Data > len(tx.Data) {
		return nil, fmt.Errorf("tx data index %d out of bounds", ps.Indexes.Data)
	}
	if ps.Indexes.Value > len(tx.Value) {
		return nil, fmt.Errorf("tx value index %d out of bounds", ps.Indexes.Value)
	}
	if ps.Indexes.Gas > len(tx.GasLimit) {
		return nil, fmt.Errorf("tx gas limit index %d out of bounds", ps.Indexes.Gas)
	}
	dataHex := tx.Data[ps.Indexes.Data]
	valueHex := tx.Value[ps.Indexes.Value]
	gasLimit := tx.GasLimit[ps.Indexes.Gas]
	// Value, Data hex encoding is messy: https://github.com/ethereum/tests/issues/203
	value := new(big.Int)
	if valueHex != "0x" {
		v, ok := math.ParseBig256(valueHex)
		if !ok {
			return nil, fmt.Errorf("invalid tx value %q", valueHex)
		}
		value = v
	}
	data, err := hex.DecodeString(strings.TrimPrefix(dataHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid tx data %q", dataHex)
	}
	var accessList types.AccessList
	if tx.AccessLists != nil && tx.AccessLists[ps.Indexes.Data] != nil {
		accessList = *tx.AccessLists[ps.Indexes.Data]
	}
	// If baseFee provided, set gasPrice to effectiveGasPrice.
	gasPrice := tx.GasPrice
	if baseFee != nil {
		if tx.MaxFeePerGas == nil {
			tx.MaxFeePerGas = gasPrice
		}
		if tx.MaxFeePerGas == nil {
			tx.MaxFeePerGas = new(big.Int)
		}
		if tx.MaxPriorityFeePerGas == nil {
			tx.MaxPriorityFeePerGas = tx.MaxFeePerGas
		}
		gasPrice = math.BigMin(new(big.Int).Add(tx.MaxPriorityFeePerGas, baseFee),
			tx.MaxFeePerGas)
	}
	if gasPrice == nil {
		return nil, errors.New("no gas price provided")
	}

	msg := &core.Message{
		From:          from,
		To:            to,
		Nonce:         tx.Nonce,
		Value:         value,
		GasLimit:      gasLimit,
		GasPrice:      gasPrice,
		GasFeeCap:     tx.MaxFeePerGas,
		GasTipCap:     tx.MaxPriorityFeePerGas,
		Data:          data,
		AccessList:    accessList,
		BlobHashes:    tx.BlobVersionedHashes,
		BlobGasFeeCap: tx.BlobGasFeeCap,
	}
	return msg, nil
}

func rlpHash(x interface{}) (h common.Hash) {
	hw := sha3.NewLegacyKeccak256()
	rlp.Encode(hw, x)
	hw.Sum(h[:0])
	return h
}

func vmTestBlockHash(n uint64) common.Hash {
	return common.BytesToHash(crypto.Keccak256([]byte(big.NewInt(int64(n)).String())))
}

// StateTestState groups all the state database objects together for use in tests.
type StateTestState struct {
	StateDB   *state.StateDB
	TrieDB    *triedb.Database
	Snapshots *snapshot.Tree
	TempDir   string
}

// MakePreState creates a state containing the given allocation.
func MakePreState(db ethdb.Database, accounts types.GenesisAlloc, snapshotter bool, scheme string) StateTestState {
	// Set database path
	tempdir, err := os.MkdirTemp("", "coreth-state-test-*")
	if err != nil {
		panic("failed to create temporary directory: " + err.Error())
	}

	tconf := &triedb.Config{Preimages: true}
	switch scheme {
	case rawdb.HashScheme:
		tconf.DBOverride = hashdb.Defaults.BackendConstructor
	case rawdb.PathScheme:
		tconf.DBOverride = pathdb.Defaults.BackendConstructor
	case customrawdb.FirewoodScheme:
		cfg := firewood.Defaults
		cfg.FilePath = filepath.Join(tempdir, "firewood")
		tconf.DBOverride = cfg.BackendConstructor
	default:
		panic("unknown trie database scheme" + scheme)
	}

	triedb := triedb.NewDatabase(db, tconf)
	sdb := state.NewDatabaseWithNodeDB(db, triedb)
	statedb, _ := state.New(types.EmptyRootHash, sdb, nil)
	for addr, a := range accounts {
		statedb.SetCode(addr, a.Code)
		statedb.SetNonce(addr, a.Nonce)
		statedb.SetBalance(addr, uint256.MustFromBig(a.Balance))
		for k, v := range a.Storage {
			statedb.SetState(addr, k, v)
		}
	}
	// Commit and re-open to start with a clean state.
	root, err := statedb.Commit(0, false)
	if err != nil {
		panic("failed to commit state: " + err.Error())
	}

	// If snapshot is requested, initialize the snapshotter and use it in state.
	var snaps *snapshot.Tree
	if snapshotter {
		snapconfig := snapshot.Config{
			CacheSize:  1,
			NoBuild:    false,
			AsyncBuild: false,
			SkipVerify: true,
		}
		snaps, _ = snapshot.New(snapconfig, db, triedb, common.Hash{}, root)
	}
	statedb, _ = state.New(root, sdb, snaps)
	return StateTestState{statedb, triedb, snaps, tempdir}
}

// Close should be called when the state is no longer needed, ie. after running the test.
func (st *StateTestState) Close() {
	if st.TrieDB != nil {
		st.TrieDB.Close()
		st.TrieDB = nil
	}
	if st.Snapshots != nil {
		// Need to call Disable here to quit the snapshot generator goroutine.
		st.Snapshots.AbortGeneration()
		st.Snapshots.Release()
		st.Snapshots = nil
	}

	if st.TempDir != "" {
		if err := os.RemoveAll(st.TempDir); err != nil {
			panic("failed to remove temporary directory: " + err.Error())
		}
		st.TempDir = ""
	}
}
