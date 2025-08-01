// Copyright (C) 2019-2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package deployerallowlisttest

import (
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/subnet-evm/precompile/allowlist/allowlisttest"
	"github.com/ava-labs/subnet-evm/precompile/contracts/deployerallowlist"
	"github.com/ava-labs/subnet-evm/precompile/precompileconfig"
	"github.com/ava-labs/subnet-evm/precompile/precompiletest"
	"github.com/ava-labs/subnet-evm/utils"
	"go.uber.org/mock/gomock"
)

func TestVerify(t *testing.T) {
	allowlisttest.VerifyPrecompileWithAllowListTests(t, deployerallowlist.Module, nil)
}

func TestEqual(t *testing.T) {
	admins := []common.Address{allowlisttest.TestAdminAddr}
	enableds := []common.Address{allowlisttest.TestEnabledAddr}
	managers := []common.Address{allowlisttest.TestManagerAddr}
	tests := map[string]precompiletest.ConfigEqualTest{
		"non-nil config and nil other": {
			Config:   deployerallowlist.NewConfig(utils.NewUint64(3), admins, enableds, managers),
			Other:    nil,
			Expected: false,
		},
		"different type": {
			Config:   deployerallowlist.NewConfig(nil, nil, nil, nil),
			Other:    precompileconfig.NewMockConfig(gomock.NewController(t)),
			Expected: false,
		},
		"different timestamp": {
			Config:   deployerallowlist.NewConfig(utils.NewUint64(3), admins, enableds, managers),
			Other:    deployerallowlist.NewConfig(utils.NewUint64(4), admins, enableds, managers),
			Expected: false,
		},
		"same config": {
			Config:   deployerallowlist.NewConfig(utils.NewUint64(3), admins, enableds, managers),
			Other:    deployerallowlist.NewConfig(utils.NewUint64(3), admins, enableds, managers),
			Expected: true,
		},
	}
	allowlisttest.EqualPrecompileWithAllowListTests(t, deployerallowlist.Module, tests)
}
