// Copyright (C) 2019-2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Code generated
// This file is a generated precompile contract config with stubbed abstract functions.
// The file is generated by a template. Please inspect every code and comment in this file before use.

package feemanager

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/subnet-evm/commontype"
	"github.com/ava-labs/subnet-evm/precompile/contract"
)

// FeeConfigChangedEventGasCost is the gas cost of a FeeConfigChanged event.
// It is the base gas cost + the gas cost of the topics (signature, sender)
// and the gas cost of the non-indexed data len(oldConfig) + len(newConfig).
const FeeConfigChangedEventGasCost = GetFeeConfigGasCost + contract.LogGas + contract.LogTopicGas*2 + 2*(feeConfigInputLen)*contract.LogDataGas

// changeFeeConfigEventData represents a ChangeFeeConfig non-indexed event data raised by the contract.
// This represents a different struct than commontype.FeeConfig, because in the contract TargetBlockRate is defined as uint256.
// uint256 must be unpacked into *big.Int
type changeFeeConfigEventData struct {
	GasLimit                 *big.Int
	TargetBlockRate          *big.Int
	MinBaseFee               *big.Int
	TargetGas                *big.Int
	BaseFeeChangeDenominator *big.Int
	MinBlockGasCost          *big.Int
	MaxBlockGasCost          *big.Int
	BlockGasCostStep         *big.Int
}

// PackFeeConfigChangedEvent packs the event into the appropriate arguments for changeFeeConfig.
// It returns topic hashes and the encoded non-indexed data.
func PackFeeConfigChangedEvent(sender common.Address, oldConfig commontype.FeeConfig, newConfig commontype.FeeConfig) ([]common.Hash, []byte, error) {
	oldConfigC := convertFromCommonConfig(oldConfig)
	newConfigC := convertFromCommonConfig(newConfig)
	return FeeManagerABI.PackEvent("FeeConfigChanged", sender, oldConfigC, newConfigC)
}

// UnpackFeeConfigChangedEventData attempts to unpack non-indexed [dataBytes].
func UnpackFeeConfigChangedEventData(dataBytes []byte) (commontype.FeeConfig, commontype.FeeConfig, error) {
	eventData := make([]changeFeeConfigEventData, 2)
	err := FeeManagerABI.UnpackIntoInterface(&eventData, "FeeConfigChanged", dataBytes)
	if err != nil {
		return commontype.FeeConfig{}, commontype.FeeConfig{}, err
	}
	return convertToCommonConfig(eventData[0]), convertToCommonConfig(eventData[1]), err
}

func convertFromCommonConfig(config commontype.FeeConfig) changeFeeConfigEventData {
	return changeFeeConfigEventData{
		GasLimit:                 config.GasLimit,
		TargetBlockRate:          new(big.Int).SetUint64(config.TargetBlockRate),
		MinBaseFee:               config.MinBaseFee,
		TargetGas:                config.TargetGas,
		BaseFeeChangeDenominator: config.BaseFeeChangeDenominator,
		MinBlockGasCost:          config.MinBlockGasCost,
		MaxBlockGasCost:          config.MaxBlockGasCost,
		BlockGasCostStep:         config.BlockGasCostStep,
	}
}

func convertToCommonConfig(config changeFeeConfigEventData) commontype.FeeConfig {
	var targetBlockRate uint64
	if config.TargetBlockRate != nil {
		targetBlockRate = config.TargetBlockRate.Uint64()
	}
	return commontype.FeeConfig{
		GasLimit:                 config.GasLimit,
		TargetBlockRate:          targetBlockRate,
		MinBaseFee:               config.MinBaseFee,
		TargetGas:                config.TargetGas,
		BaseFeeChangeDenominator: config.BaseFeeChangeDenominator,
		MinBlockGasCost:          config.MinBlockGasCost,
		MaxBlockGasCost:          config.MaxBlockGasCost,
		BlockGasCostStep:         config.BlockGasCostStep,
	}
}
