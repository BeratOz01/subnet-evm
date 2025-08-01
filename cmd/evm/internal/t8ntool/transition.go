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
// Copyright 2020 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package t8ntool

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/eth/tracers/logger"
	"github.com/ava-labs/libevm/log"
	"github.com/ava-labs/subnet-evm/core/state"
	"github.com/ava-labs/subnet-evm/eth/tracers"
	"github.com/ava-labs/subnet-evm/params"
	customheader "github.com/ava-labs/subnet-evm/plugin/evm/header"
	"github.com/ava-labs/subnet-evm/plugin/evm/upgrade/subnetevm"
	"github.com/ava-labs/subnet-evm/tests"
	"github.com/urfave/cli/v2"
)

const (
	ErrorEVM              = 2
	ErrorConfig           = 3
	ErrorMissingBlockhash = 4

	ErrorJson = 10
	ErrorIO   = 11
	ErrorRlp  = 12

	stdinSelector = "stdin"
)

type NumberedError struct {
	errorCode int
	err       error
}

func NewError(errorCode int, err error) *NumberedError {
	return &NumberedError{errorCode, err}
}

func (n *NumberedError) Error() string {
	return fmt.Sprintf("ERROR(%d): %v", n.errorCode, n.err.Error())
}

func (n *NumberedError) ExitCode() int {
	return n.errorCode
}

// compile-time conformance test
var (
	_ cli.ExitCoder = (*NumberedError)(nil)
)

type input struct {
	Alloc types.GenesisAlloc `json:"alloc,omitempty"`
	Env   *stEnv             `json:"env,omitempty"`
	Txs   []*txWithKey       `json:"txs,omitempty"`
	TxRlp string             `json:"txsRlp,omitempty"`
}

func Transition(ctx *cli.Context) error {
	var getTracer = func(txIndex int, txHash common.Hash) (vm.EVMLogger, error) { return nil, nil }

	baseDir, err := createBasedir(ctx)
	if err != nil {
		return NewError(ErrorIO, fmt.Errorf("failed creating output basedir: %v", err))
	}

	if ctx.Bool(TraceFlag.Name) { // JSON opcode tracing
		// Configure the EVM logger
		logConfig := &logger.Config{
			DisableStack:     ctx.Bool(TraceDisableStackFlag.Name),
			EnableMemory:     ctx.Bool(TraceEnableMemoryFlag.Name),
			EnableReturnData: ctx.Bool(TraceEnableReturnDataFlag.Name),
			Debug:            true,
		}
		getTracer = func(txIndex int, txHash common.Hash) (vm.EVMLogger, error) {
			traceFile, err := os.Create(path.Join(baseDir, fmt.Sprintf("trace-%d-%v.jsonl", txIndex, txHash.String())))
			if err != nil {
				return nil, NewError(ErrorIO, fmt.Errorf("failed creating trace-file: %v", err))
			}
			return &traceWriter{logger.NewJSONLogger(logConfig, traceFile), traceFile}, nil
		}
	} else if ctx.IsSet(TraceTracerFlag.Name) {
		var config json.RawMessage
		if ctx.IsSet(TraceTracerConfigFlag.Name) {
			config = []byte(ctx.String(TraceTracerConfigFlag.Name))
		}
		getTracer = func(txIndex int, txHash common.Hash) (vm.EVMLogger, error) {
			traceFile, err := os.Create(path.Join(baseDir, fmt.Sprintf("trace-%d-%v.json", txIndex, txHash.String())))
			if err != nil {
				return nil, NewError(ErrorIO, fmt.Errorf("failed creating trace-file: %v", err))
			}
			tracer, err := tracers.DefaultDirectory.New(ctx.String(TraceTracerFlag.Name), nil, config)
			if err != nil {
				return nil, NewError(ErrorConfig, fmt.Errorf("failed instantiating tracer: %w", err))
			}
			return &traceWriter{tracer, traceFile}, nil
		}
	}
	// We need to load three things: alloc, env and transactions. May be either in
	// stdin input or in files.
	// Check if anything needs to be read from stdin
	var (
		prestate Prestate
		txIt     txIterator // txs to apply
		allocStr = ctx.String(InputAllocFlag.Name)

		envStr    = ctx.String(InputEnvFlag.Name)
		txStr     = ctx.String(InputTxsFlag.Name)
		inputData = &input{}
	)
	// Figure out the prestate alloc
	if allocStr == stdinSelector || envStr == stdinSelector || txStr == stdinSelector {
		decoder := json.NewDecoder(os.Stdin)
		if err := decoder.Decode(inputData); err != nil {
			return NewError(ErrorJson, fmt.Errorf("failed unmarshaling stdin: %v", err))
		}
	}
	if allocStr != stdinSelector {
		if err := readFile(allocStr, "alloc", &inputData.Alloc); err != nil {
			return err
		}
	}
	prestate.Pre = inputData.Alloc

	// Set the block environment
	if envStr != stdinSelector {
		var env stEnv
		if err := readFile(envStr, "env", &env); err != nil {
			return err
		}
		inputData.Env = &env
	}
	prestate.Env = *inputData.Env

	vmConfig := vm.Config{}
	// Construct the chainconfig
	var chainConfig *params.ChainConfig
	if cConf, extraEips, err := tests.GetChainConfig(ctx.String(ForknameFlag.Name)); err != nil {
		return NewError(ErrorConfig, fmt.Errorf("failed constructing chain configuration: %v", err))
	} else {
		chainConfig = cConf
		vmConfig.ExtraEips = extraEips
	}
	// Set the chain id
	chainConfig.ChainID = big.NewInt(ctx.Int64(ChainIDFlag.Name))

	if txIt, err = loadTransactions(txStr, inputData, prestate.Env, chainConfig); err != nil {
		return err
	}
	if err := applyLondonChecks(&prestate.Env, chainConfig); err != nil {
		return err
	}
	// NOTE: Removed isMerged logic here.
	if prestate.Env.Random != nil {
		// NOTE: subnet-evm continues to return the difficulty value for the RANDOM opcode,
		// so for testing if Random is set in the environment, we copy it to difficulty instead.
		prestate.Env.Difficulty = prestate.Env.Random
	}
	if err := applyCancunChecks(&prestate.Env, chainConfig); err != nil {
		return err
	}
	// Run the test and aggregate the result
	s, result, body, err := prestate.Apply(vmConfig, chainConfig, txIt, ctx.Int64(RewardFlag.Name), getTracer)
	if err != nil {
		return err
	}
	// Dump the execution result
	collector := make(Alloc)
	s.DumpToCollector(collector, nil)
	return dispatchOutput(ctx, baseDir, result, collector, body)
}

func applyLondonChecks(env *stEnv, chainConfig *params.ChainConfig) error {
	if !chainConfig.IsLondon(new(big.Int).SetUint64(env.Number)) {
		return nil
	}
	// Sanity check, to not `panic` in state_transition
	if env.BaseFee != nil {
		// Already set, base fee has precedent over parent base fee.
		return nil
	}
	if env.ParentBaseFee == nil || env.Number == 0 {
		return NewError(ErrorConfig, errors.New("EIP-1559 config but missing 'currentBaseFee' in env section"))
	}
	parent := &types.Header{
		Number:   new(big.Int).SetUint64(env.Number - 1),
		Time:     env.ParentTimestamp,
		BaseFee:  env.ParentBaseFee,
		GasUsed:  env.ParentGasUsed,
		GasLimit: env.ParentGasLimit,
		Extra:    make([]byte, subnetevm.WindowSize), // TODO: consider passing extra through env
	}
	feeConfig := params.DefaultFeeConfig
	if env.MinBaseFee != nil {
		// Override the default min base fee if it's set in the env
		feeConfig.MinBaseFee = env.MinBaseFee
	}
	configExtra := params.GetExtra(chainConfig)
	var err error
	env.BaseFee, err = customheader.BaseFee(configExtra, feeConfig, parent, env.Timestamp)
	if err != nil {
		return NewError(ErrorConfig, fmt.Errorf("failed calculating base fee: %v", err))
	}
	return nil
}

func applyCancunChecks(env *stEnv, chainConfig *params.ChainConfig) error {
	if !chainConfig.IsCancun(big.NewInt(int64(env.Number)), env.Timestamp) {
		env.ParentBeaconBlockRoot = nil // un-set it if it has been set too early
		return nil
	}
	// Post-cancun
	// We require EIP-4788 beacon root to be set in the env
	if env.ParentBeaconBlockRoot == nil {
		return NewError(ErrorConfig, errors.New("post-cancun env requires parentBeaconBlockRoot to be set"))
	}
	return nil
}

type Alloc map[common.Address]types.Account

func (g Alloc) OnRoot(common.Hash) {}

func (g Alloc) OnAccount(addr *common.Address, dumpAccount state.DumpAccount) {
	if addr == nil {
		return
	}
	balance, _ := new(big.Int).SetString(dumpAccount.Balance, 0)
	var storage map[common.Hash]common.Hash
	if dumpAccount.Storage != nil {
		storage = make(map[common.Hash]common.Hash)
		for k, v := range dumpAccount.Storage {
			storage[k] = common.HexToHash(v)
		}
	}
	genesisAccount := types.Account{
		Code:    dumpAccount.Code,
		Storage: storage,
		Balance: balance,
		Nonce:   dumpAccount.Nonce,
	}
	g[*addr] = genesisAccount
}

// saveFile marshals the object to the given file
func saveFile(baseDir, filename string, data interface{}) error {
	b, err := json.MarshalIndent(data, "", " ")
	if err != nil {
		return NewError(ErrorJson, fmt.Errorf("failed marshalling output: %v", err))
	}
	location := path.Join(baseDir, filename)
	if err = os.WriteFile(location, b, 0644); err != nil {
		return NewError(ErrorIO, fmt.Errorf("failed writing output: %v", err))
	}
	log.Info("Wrote file", "file", location)
	return nil
}

// dispatchOutput writes the output data to either stderr or stdout, or to the specified
// files
func dispatchOutput(ctx *cli.Context, baseDir string, result *ExecutionResult, alloc Alloc, body hexutil.Bytes) error {
	stdOutObject := make(map[string]interface{})
	stdErrObject := make(map[string]interface{})
	dispatch := func(baseDir, fName, name string, obj interface{}) error {
		switch fName {
		case "stdout":
			stdOutObject[name] = obj
		case "stderr":
			stdErrObject[name] = obj
		case "":
			// don't save
		default: // save to file
			if err := saveFile(baseDir, fName, obj); err != nil {
				return err
			}
		}
		return nil
	}
	if err := dispatch(baseDir, ctx.String(OutputAllocFlag.Name), "alloc", alloc); err != nil {
		return err
	}
	if err := dispatch(baseDir, ctx.String(OutputResultFlag.Name), "result", result); err != nil {
		return err
	}
	if err := dispatch(baseDir, ctx.String(OutputBodyFlag.Name), "body", body); err != nil {
		return err
	}
	if len(stdOutObject) > 0 {
		b, err := json.MarshalIndent(stdOutObject, "", "  ")
		if err != nil {
			return NewError(ErrorJson, fmt.Errorf("failed marshalling output: %v", err))
		}
		os.Stdout.Write(b)
		os.Stdout.WriteString("\n")
	}
	if len(stdErrObject) > 0 {
		b, err := json.MarshalIndent(stdErrObject, "", "  ")
		if err != nil {
			return NewError(ErrorJson, fmt.Errorf("failed marshalling output: %v", err))
		}
		os.Stderr.Write(b)
		os.Stderr.WriteString("\n")
	}
	return nil
}
