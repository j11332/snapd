// -*- Mode: Go; indent-tabs-mode: t -*-
// +build !nosecboot

/*
 * Copyright (C) 2020 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package secboot_test

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/canonical/go-tpm2"
	sb "github.com/snapcore/secboot"
	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/bootloader"
	"github.com/snapcore/snapd/bootloader/efi"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/osutil/disks"
	"github.com/snapcore/snapd/secboot"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/snapfile"
	"github.com/snapcore/snapd/snap/squashfs"
	"github.com/snapcore/snapd/testutil"
)

func TestSecboot(t *testing.T) { TestingT(t) }

type secbootSuite struct {
	testutil.BaseTest
}

var _ = Suite(&secbootSuite{})

func (s *secbootSuite) SetUpTest(c *C) {
	dirs.SetRootDir(c.MkDir())
	s.AddCleanup(func() { dirs.SetRootDir("/") })
}

func (s *secbootSuite) TestCheckKeySealingSupported(c *C) {
	sbEmpty := []uint8{}
	sbEnabled := []uint8{1}
	sbDisabled := []uint8{0}
	efiNotSupported := []uint8(nil)
	tpmErr := errors.New("TPM error")

	type testCase struct {
		tpmErr     error
		tpmEnabled bool
		sbData     []uint8
		err        string
	}
	for i, tc := range []testCase{
		// happy case
		{tpmErr: nil, tpmEnabled: true, sbData: sbEnabled, err: ""},
		// secure boot EFI var is empty
		{tpmErr: nil, tpmEnabled: true, sbData: sbEmpty, err: "secure boot variable does not exist"},
		// secure boot is disabled
		{tpmErr: nil, tpmEnabled: true, sbData: sbDisabled, err: "secure boot is disabled"},
		// EFI not supported
		{tpmErr: nil, tpmEnabled: true, sbData: efiNotSupported, err: "not a supported EFI system"},
		// TPM connection error
		{tpmErr: tpmErr, sbData: sbEnabled, err: "cannot connect to TPM device: TPM error"},
		// TPM was detected but it's not enabled
		{tpmErr: nil, tpmEnabled: false, sbData: sbEnabled, err: "TPM device is not enabled"},
		// No TPM device
		{tpmErr: sb.ErrNoTPM2Device, sbData: sbEnabled, err: "cannot connect to TPM device: no TPM2 device is available"},
	} {
		c.Logf("%d: %v %v %v %q", i, tc.tpmErr, tc.tpmEnabled, tc.sbData, tc.err)

		_, restore := mockSbTPMConnection(c, tc.tpmErr)
		defer restore()

		restore = secboot.MockIsTPMEnabled(func(tpm *sb.TPMConnection) bool {
			return tc.tpmEnabled
		})
		defer restore()

		var vars map[string][]byte
		if tc.sbData != nil {
			vars = map[string][]byte{"SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c": tc.sbData}
		}
		restoreEfiVars := efi.MockVars(vars, nil)
		defer restoreEfiVars()

		err := secboot.CheckKeySealingSupported()
		if tc.err == "" {
			c.Assert(err, IsNil)
		} else {
			c.Assert(err, ErrorMatches, tc.err)
		}
	}
}

func (s *secbootSuite) TestMeasureSnapSystemEpochWhenPossible(c *C) {
	for _, tc := range []struct {
		tpmErr     error
		tpmEnabled bool
		callNum    int
		err        string
	}{
		{
			// normal connection to the TPM device
			tpmErr: nil, tpmEnabled: true, callNum: 1, err: "",
		},
		{
			// TPM device exists but returns error
			tpmErr: errors.New("tpm error"), callNum: 0,
			err: "cannot measure snap system epoch: cannot open TPM connection: tpm error",
		},
		{
			// TPM device exists but is disabled
			tpmErr: nil, tpmEnabled: false,
		},
		{
			// TPM device does not exist
			tpmErr: sb.ErrNoTPM2Device,
		},
	} {
		mockTpm, restore := mockSbTPMConnection(c, tc.tpmErr)
		defer restore()

		restore = secboot.MockIsTPMEnabled(func(tpm *sb.TPMConnection) bool {
			return tc.tpmEnabled
		})
		defer restore()

		calls := 0
		restore = secboot.MockSbMeasureSnapSystemEpochToTPM(func(tpm *sb.TPMConnection, pcrIndex int) error {
			calls++
			c.Assert(tpm, Equals, mockTpm)
			c.Assert(pcrIndex, Equals, 12)
			return nil
		})
		defer restore()

		err := secboot.MeasureSnapSystemEpochWhenPossible()
		if tc.err == "" {
			c.Assert(err, IsNil)
		} else {
			c.Assert(err, ErrorMatches, tc.err)
		}
		c.Assert(calls, Equals, tc.callNum)
	}
}

func (s *secbootSuite) TestMeasureSnapModelWhenPossible(c *C) {
	for i, tc := range []struct {
		tpmErr     error
		tpmEnabled bool
		modelErr   error
		callNum    int
		err        string
	}{
		{
			// normal connection to the TPM device
			tpmErr: nil, tpmEnabled: true, modelErr: nil, callNum: 1, err: "",
		},
		{
			// normal connection to the TPM device with model error
			tpmErr: nil, tpmEnabled: true, modelErr: errors.New("model error"), callNum: 0,
			err: "cannot measure snap model: model error",
		},
		{
			// TPM device exists but returns error
			tpmErr: errors.New("tpm error"), callNum: 0,
			err: "cannot measure snap model: cannot open TPM connection: tpm error",
		},
		{
			// TPM device exists but is disabled
			tpmErr: nil, tpmEnabled: false,
		},
		{
			// TPM device does not exist
			tpmErr: sb.ErrNoTPM2Device,
		},
	} {
		c.Logf("%d: tpmErr:%v tpmEnabled:%v", i, tc.tpmErr, tc.tpmEnabled)
		mockModel := &asserts.Model{}

		mockTpm, restore := mockSbTPMConnection(c, tc.tpmErr)
		defer restore()

		restore = secboot.MockIsTPMEnabled(func(tpm *sb.TPMConnection) bool {
			return tc.tpmEnabled
		})
		defer restore()

		calls := 0
		restore = secboot.MockSbMeasureSnapModelToTPM(func(tpm *sb.TPMConnection, pcrIndex int, model sb.SnapModel) error {
			calls++
			c.Assert(tpm, Equals, mockTpm)
			c.Assert(model, Equals, mockModel)
			c.Assert(pcrIndex, Equals, 12)
			return nil
		})
		defer restore()

		findModel := func() (*asserts.Model, error) {
			if tc.modelErr != nil {
				return nil, tc.modelErr
			}
			return mockModel, nil
		}

		err := secboot.MeasureSnapModelWhenPossible(findModel)
		if tc.err == "" {
			c.Assert(err, IsNil)
		} else {
			c.Assert(err, ErrorMatches, tc.err)
		}
		c.Assert(calls, Equals, tc.callNum)
	}
}

func (s *secbootSuite) TestLockTPMSealedKeys(c *C) {
	tt := []struct {
		tpmErr     error
		tpmEnabled bool
		lockOk     bool
		expError   string
	}{
		// can't connect to tpm
		{
			tpmErr:   fmt.Errorf("failed to connect to tpm"),
			expError: "cannot lock TPM: failed to connect to tpm",
		},
		// tpm is not enabled, no errors
		{
			tpmEnabled: false,
		},
		// can't lock pcr protection profile
		{
			lockOk:     false,
			tpmEnabled: true,
			expError:   "block failed",
		},
		// tpm enabled, we can lock it
		{
			lockOk:     true,
			tpmEnabled: true,
		},
	}

	for _, tc := range tt {
		mockSbTPM, restoreConnect := mockSbTPMConnection(c, tc.tpmErr)
		defer restoreConnect()

		restore := secboot.MockIsTPMEnabled(func(tpm *sb.TPMConnection) bool {
			return tc.tpmEnabled
		})
		defer restore()

		sbBlockPCRProtectionPolicesCalls := 0
		restore = secboot.MockSbBlockPCRProtectionPolicies(func(tpm *sb.TPMConnection, pcrs []int) error {
			sbBlockPCRProtectionPolicesCalls++
			c.Assert(tpm, Equals, mockSbTPM)
			c.Assert(pcrs, DeepEquals, []int{12})
			if tc.lockOk {
				return nil
			}
			return errors.New("block failed")
		})
		defer restore()

		err := secboot.LockTPMSealedKeys()
		if tc.expError != "" {
			c.Assert(err, ErrorMatches, tc.expError)
			// if there was not a tpm error, we should have locked it
			if tc.tpmErr == nil {
				c.Assert(sbBlockPCRProtectionPolicesCalls, Equals, 1)
			} else {
				c.Assert(sbBlockPCRProtectionPolicesCalls, Equals, 0)
			}
		} else {
			c.Assert(err, IsNil)
			// if the tpm was enabled, we should have locked it
			if tc.tpmEnabled {
				c.Assert(sbBlockPCRProtectionPolicesCalls, Equals, 1)
			} else {
				c.Assert(sbBlockPCRProtectionPolicesCalls, Equals, 0)
			}
		}
	}
}

func (s *secbootSuite) TestUnlockVolumeUsingSealedKeyIfEncrypted(c *C) {

	// setup mock disks to use for locating the partition
	// restore := disks.MockMountPointDisksToPartitionMapping()
	// defer restore()

	mockDiskWithEncDev := &disks.MockDiskMapping{
		FilesystemLabelToPartUUID: map[string]string{
			"name-enc": "enc-dev-partuuid",
		},
	}

	mockDiskWithoutAnyDev := &disks.MockDiskMapping{
		FilesystemLabelToPartUUID: map[string]string{},
	}

	mockDiskWithUnencDev := &disks.MockDiskMapping{
		FilesystemLabelToPartUUID: map[string]string{
			"name": "unenc-dev-partuuid",
		},
	}

	for idx, tc := range []struct {
		tpmErr              error
		keyfile             string // the keyfile to be used to unseal
		tpmEnabled          bool   // TPM storage and endorsement hierarchies disabled, only relevant if TPM available
		hasEncdev           bool   // an encrypted device exists
		rkAllow             bool   // allow recovery key activation
		rkErr               error  // recovery key unlock error, only relevant if TPM not available
		lockRequest         bool   // request to lock access to the sealed key, only relevant if TPM available
		lockOk              bool   // the lock operation succeeded
		activated           bool   // the activation operation succeeded
		activateErr         error  // the activation error
		err                 string
		skipDiskEnsureCheck bool // whether to check to ensure the mock disk contains the device label
		expUnlockMethod     secboot.UnlockMethod
		disk                *disks.MockDiskMapping
	}{
		{
			// happy case with tpm and encrypted device (lock requested)
			tpmEnabled: true, hasEncdev: true, lockRequest: true, lockOk: true,
			activated:       true,
			disk:            mockDiskWithEncDev,
			expUnlockMethod: secboot.UnlockedWithSealedKey,
		}, {
			// happy case with tpm and encrypted device (lock requested) with
			// an alternative keyfile
			tpmEnabled: true, hasEncdev: true, lockRequest: true, lockOk: true,
			activated:       true,
			disk:            mockDiskWithEncDev,
			keyfile:         "some-other-keyfile",
			expUnlockMethod: secboot.UnlockedWithSealedKey,
		}, {
			// device activation fails (lock requested)
			tpmEnabled: true, hasEncdev: true, lockRequest: true, lockOk: true,
			err:  "cannot activate encrypted device .*: activation error",
			disk: mockDiskWithEncDev,
		}, {
			// activation works but PCR policy block fails (lock requested)
			tpmEnabled: true, hasEncdev: true, lockRequest: true, activated: true,
			err:             "cannot lock access to sealed keys: block failed",
			disk:            mockDiskWithEncDev,
			expUnlockMethod: secboot.UnlockedWithSealedKey,
		}, {
			// happy case with tpm and encrypted device
			tpmEnabled: true, hasEncdev: true, lockOk: true, activated: true,
			disk:            mockDiskWithEncDev,
			expUnlockMethod: secboot.UnlockedWithSealedKey,
		}, {
			// device activation fails
			tpmEnabled: true, hasEncdev: true,
			err:  "cannot activate encrypted device .*: activation error",
			disk: mockDiskWithEncDev,
		}, {
			// activation works but lock fails
			tpmEnabled: true, hasEncdev: true, activated: true,
			disk:            mockDiskWithEncDev,
			expUnlockMethod: secboot.UnlockedWithSealedKey,
		}, {
			// happy case without encrypted device (lock requested)
			tpmEnabled: true, lockRequest: true, lockOk: true,
			disk: mockDiskWithUnencDev,
		}, {
			// happy case with tpm and encrypted device, activation
			// with recovery key
			tpmEnabled: true, hasEncdev: true, lockOk: true, activated: true,
			activateErr: &sb.ActivateWithTPMSealedKeyError{
				// activation error with nil recovery key error
				// implies volume activated successfully using
				// the recovery key,
				RecoveryKeyUsageErr: nil,
			},
			disk:            mockDiskWithEncDev,
			expUnlockMethod: secboot.UnlockedWithRecoveryKey,
		}, {
			// tpm and encrypted device, successful activation, but
			// recovery key non-nil is an unexpected state
			tpmEnabled: true, hasEncdev: true, lockOk: true, activated: true,
			activateErr: &sb.ActivateWithTPMSealedKeyError{
				RecoveryKeyUsageErr: fmt.Errorf("unexpected"),
			},
			expUnlockMethod: secboot.UnlockStatusUnknown,
			err:             `internal error: volume activated with unexpected error: .* \(unexpected\)`,
			disk:            mockDiskWithEncDev,
		}, {
			// activation works but lock fails, without encrypted device (lock requested)
			tpmEnabled: true, lockRequest: true, activated: true,
			err:  "cannot lock access to sealed keys: block failed",
			disk: mockDiskWithUnencDev,
		}, {
			// happy case without encrypted device
			tpmEnabled: true, lockOk: true,
			disk: mockDiskWithUnencDev,
		}, {
			// activation works but lock fails, no encrypted device
			tpmEnabled: true,
			disk:       mockDiskWithUnencDev,
		}, {
			// tpm error, no encrypted device
			tpmErr: errors.New("tpm error"),
			err:    `cannot unlock encrypted device "name": tpm error`,
			disk:   mockDiskWithUnencDev,
		}, {
			// tpm error, has encrypted device
			tpmErr: errors.New("tpm error"), hasEncdev: true,
			err:  `cannot unlock encrypted device "name": tpm error`,
			disk: mockDiskWithEncDev,
		}, {
			// tpm disabled, no encrypted device
			disk: mockDiskWithUnencDev,
		}, {
			// tpm disabled, has encrypted device, unlocked using the recovery key
			hasEncdev:       true,
			rkAllow:         true,
			disk:            mockDiskWithEncDev,
			expUnlockMethod: secboot.UnlockedWithRecoveryKey,
		}, {
			// tpm disabled, has encrypted device, recovery key unlocking fails
			hasEncdev: true, rkErr: errors.New("cannot unlock with recovery key"),
			rkAllow: true,
			disk:    mockDiskWithEncDev,
			err:     `cannot unlock encrypted device ".*/enc-dev-partuuid": cannot unlock with recovery key`,
		}, {
			// no tpm, has encrypted device, unlocked using the recovery key (lock requested)
			tpmErr: sb.ErrNoTPM2Device, hasEncdev: true, lockRequest: true,
			rkAllow:         true,
			disk:            mockDiskWithEncDev,
			expUnlockMethod: secboot.UnlockedWithRecoveryKey,
		}, {
			// no tpm, has encrypted device, unlocking with recovery key not allowed
			tpmErr: sb.ErrNoTPM2Device, hasEncdev: true, lockRequest: true,
			disk: mockDiskWithEncDev,
			err:  `cannot activate encrypted device ".*/enc-dev-partuuid": activation error`,
		}, {
			// no tpm, has encrypted device, recovery key unlocking fails
			rkErr:  errors.New("cannot unlock with recovery key"),
			tpmErr: sb.ErrNoTPM2Device, hasEncdev: true, lockRequest: true,
			rkAllow: true,
			disk:    mockDiskWithEncDev,
			err:     `cannot unlock encrypted device ".*/enc-dev-partuuid": cannot unlock with recovery key`,
		}, {
			// no tpm, has encrypted device, unlocked using the recovery key
			tpmErr: sb.ErrNoTPM2Device, hasEncdev: true, rkAllow: true,
			disk:            mockDiskWithEncDev,
			expUnlockMethod: secboot.UnlockedWithRecoveryKey,
		}, {
			// no tpm, has encrypted device, recovery key not allowed
			tpmErr: sb.ErrNoTPM2Device, hasEncdev: true,
			disk: mockDiskWithEncDev,
			err:  `cannot activate encrypted device ".*/enc-dev-partuuid": activation error`,
		}, {
			// no tpm, no encrypted device (lock requested)
			tpmErr: sb.ErrNoTPM2Device, lockRequest: true,
			disk: mockDiskWithUnencDev,
		}, {
			// no tpm, no encrypted device
			tpmErr: sb.ErrNoTPM2Device,
			disk:   mockDiskWithUnencDev,
		}, {
			// no disks at all
			disk:                mockDiskWithoutAnyDev,
			skipDiskEnsureCheck: true,
			// error is specifically for failing to find name, NOT name-enc, we
			// will properly fall back to looking for name if we didn't find
			// name-enc
			err: "error enumerating partitions for disk to find unencrypted device \"name\": filesystem label \"name\" not found",
		},
	} {
		randomUUID := fmt.Sprintf("random-uuid-for-test-%d", idx)
		restore := secboot.MockRandomKernelUUID(func() string {
			return randomUUID
		})
		defer restore()

		c.Logf("tc %v: %+v", idx, tc)
		mockSbTPM, restoreConnect := mockSbTPMConnection(c, tc.tpmErr)
		defer restoreConnect()

		restore = secboot.MockIsTPMEnabled(func(tpm *sb.TPMConnection) bool {
			return tc.tpmEnabled
		})
		defer restore()

		sbBlockPCRProtectionPolicesCalls := 0
		restore = secboot.MockSbBlockPCRProtectionPolicies(func(tpm *sb.TPMConnection, pcrs []int) error {
			sbBlockPCRProtectionPolicesCalls++
			c.Assert(tpm, Equals, mockSbTPM)
			c.Assert(pcrs, DeepEquals, []int{12})
			if tc.lockOk {
				return nil
			}
			return errors.New("block failed")
		})
		defer restore()

		defaultDevice := "name"

		fsLabel := defaultDevice
		if tc.hasEncdev {
			fsLabel += "-enc"
		}
		partuuid, ok := tc.disk.FilesystemLabelToPartUUID[fsLabel]
		if !tc.skipDiskEnsureCheck {
			c.Assert(ok, Equals, true)
		}
		devicePath := filepath.Join("/dev/disk/by-partuuid", partuuid)

		expKeyPath := tc.keyfile
		if expKeyPath == "" {
			expKeyPath = "vanilla-keyfile"
		}

		restore = secboot.MockSbActivateVolumeWithTPMSealedKey(func(tpm *sb.TPMConnection, volumeName, sourceDevicePath,
			keyPath string, pinReader io.Reader, options *sb.ActivateVolumeOptions) (bool, error) {
			c.Assert(volumeName, Equals, "name-"+randomUUID)
			c.Assert(sourceDevicePath, Equals, devicePath)
			c.Assert(keyPath, Equals, expKeyPath)
			if tc.rkAllow {
				c.Assert(*options, DeepEquals, sb.ActivateVolumeOptions{
					PassphraseTries:  1,
					RecoveryKeyTries: 3,
					KeyringPrefix:    "ubuntu-fde",
				})
			} else {
				c.Assert(*options, DeepEquals, sb.ActivateVolumeOptions{
					PassphraseTries: 1,
					// activation with recovery key was disabled
					RecoveryKeyTries: 0,
					KeyringPrefix:    "ubuntu-fde",
				})
			}
			if !tc.activated && tc.activateErr == nil {
				return false, errors.New("activation error")
			}
			return tc.activated, tc.activateErr
		})
		defer restore()

		restore = secboot.MockSbActivateVolumeWithRecoveryKey(func(name, device string, keyReader io.Reader,
			options *sb.ActivateVolumeOptions) error {
			if !tc.rkAllow {
				c.Fatalf("unexpected attempt to activate with recovery key")
				return fmt.Errorf("unexpected call")
			}
			return tc.rkErr
		})
		defer restore()

		opts := &secboot.UnlockVolumeUsingSealedKeyOptions{
			LockKeysOnFinish: tc.lockRequest,
			AllowRecoveryKey: tc.rkAllow,
		}
		unlockRes, err := secboot.UnlockVolumeUsingSealedKeyIfEncrypted(tc.disk, defaultDevice, expKeyPath, opts)
		if tc.err == "" {
			c.Assert(err, IsNil)
			c.Assert(unlockRes.IsDecryptedDevice, Equals, tc.hasEncdev)
			if tc.hasEncdev {
				c.Assert(unlockRes.Device, Equals, filepath.Join("/dev/mapper", defaultDevice+"-"+randomUUID))
			} else {
				c.Assert(unlockRes.Device, Equals, devicePath)
			}
		} else {
			c.Assert(err, ErrorMatches, tc.err)
			// also check that the isDecryptDev value matches, this is
			// important for robust callers to know whether they should try to
			// unlock using a different method or not
			// this is only skipped on some test cases where we get an error
			// very early, like trying to connect to the tpm
			c.Assert(unlockRes.IsDecryptedDevice, Equals, tc.hasEncdev)
		}

		c.Assert(unlockRes.UnlockMethod, Equals, tc.expUnlockMethod)

		// BlockPCRProtectionPolicies should be called whenever there is a TPM device
		// detected, regardless of whether secure boot is enabled or there is an
		// encrypted volume to unlock. If we have multiple encrypted volumes, we
		// should call it after the last one is unlocked.
		if tc.tpmErr == nil && tc.lockRequest {
			c.Assert(sbBlockPCRProtectionPolicesCalls, Equals, 1)
		} else {
			c.Assert(sbBlockPCRProtectionPolicesCalls, Equals, 0)
		}
	}
}

func (s *secbootSuite) TestEFIImageFromBootFile(c *C) {
	tmpDir := c.MkDir()

	// set up some test files
	existingFile := filepath.Join(tmpDir, "foo")
	err := ioutil.WriteFile(existingFile, nil, 0644)
	c.Assert(err, IsNil)
	missingFile := filepath.Join(tmpDir, "bar")
	snapFile := filepath.Join(tmpDir, "test.snap")
	snapf, err := createMockSnapFile(c.MkDir(), snapFile, "app")

	for _, tc := range []struct {
		bootFile bootloader.BootFile
		efiImage sb.EFIImage
		err      string
	}{
		{
			// happy case for EFI image
			bootFile: bootloader.NewBootFile("", existingFile, bootloader.RoleRecovery),
			efiImage: sb.FileEFIImage(existingFile),
		},
		{
			// missing EFI image
			bootFile: bootloader.NewBootFile("", missingFile, bootloader.RoleRecovery),
			err:      fmt.Sprintf("file %s/bar does not exist", tmpDir),
		},
		{
			// happy case for snap file
			bootFile: bootloader.NewBootFile(snapFile, "rel", bootloader.RoleRecovery),
			efiImage: sb.SnapFileEFIImage{Container: snapf, Path: snapFile, FileName: "rel"},
		},
		{
			// invalid snap file
			bootFile: bootloader.NewBootFile(existingFile, "rel", bootloader.RoleRecovery),
			err:      fmt.Sprintf(`"%s/foo" is not a snap or snapdir`, tmpDir),
		},
		{
			// missing snap file
			bootFile: bootloader.NewBootFile(missingFile, "rel", bootloader.RoleRecovery),
			err:      fmt.Sprintf(`"%s/bar" is not a snap or snapdir`, tmpDir),
		},
	} {
		o, err := secboot.EFIImageFromBootFile(&tc.bootFile)
		if tc.err == "" {
			c.Assert(err, IsNil)
			c.Assert(o, DeepEquals, tc.efiImage)
		} else {
			c.Assert(err, ErrorMatches, tc.err)
		}
	}
}

func (s *secbootSuite) TestSealKey(c *C) {
	mockErr := errors.New("some error")

	for _, tc := range []struct {
		tpmErr               error
		tpmEnabled           bool
		missingFile          bool
		badSnapFile          bool
		skipProvision        bool
		addEFISbPolicyErr    error
		addEFIBootManagerErr error
		addSystemdEFIStubErr error
		addSnapModelErr      error
		provisioningErr      error
		sealErr              error
		provisioningCalls    int
		sealCalls            int
		expectedErr          string
	}{
		{tpmErr: mockErr, expectedErr: "cannot connect to TPM: some error"},
		{tpmEnabled: false, expectedErr: "TPM device is not enabled"},
		{tpmEnabled: true, missingFile: true, expectedErr: "cannot build EFI image load sequences: file /does/not/exist does not exist"},
		{tpmEnabled: true, badSnapFile: true, expectedErr: `.*/kernel.snap" is not a snap or snapdir`},
		{tpmEnabled: true, addEFISbPolicyErr: mockErr, expectedErr: "cannot add EFI secure boot policy profile: some error"},
		{tpmEnabled: true, addEFIBootManagerErr: mockErr, expectedErr: "cannot add EFI boot manager profile: some error"},
		{tpmEnabled: true, addSystemdEFIStubErr: mockErr, expectedErr: "cannot add systemd EFI stub profile: some error"},
		{tpmEnabled: true, addSnapModelErr: mockErr, expectedErr: "cannot add snap model profile: some error"},
		{tpmEnabled: true, provisioningErr: mockErr, provisioningCalls: 1, expectedErr: "cannot provision TPM: some error"},
		{tpmEnabled: true, sealErr: mockErr, provisioningCalls: 1, sealCalls: 1, expectedErr: "some error"},
		{tpmEnabled: true, skipProvision: true, provisioningCalls: 0, sealCalls: 1, expectedErr: ""},
		{tpmEnabled: true, provisioningCalls: 1, sealCalls: 1, expectedErr: ""},
	} {
		tmpDir := c.MkDir()
		var mockBF []bootloader.BootFile
		for _, name := range []string{"a", "b", "c", "d"} {
			mockFileName := filepath.Join(tmpDir, name)
			err := ioutil.WriteFile(mockFileName, nil, 0644)
			c.Assert(err, IsNil)
			mockBF = append(mockBF, bootloader.NewBootFile("", mockFileName, bootloader.RoleRecovery))
		}

		if tc.missingFile {
			mockBF[0].Path = "/does/not/exist"
		}

		var kernelSnap snap.Container
		snapPath := filepath.Join(tmpDir, "kernel.snap")
		if tc.badSnapFile {
			err := ioutil.WriteFile(snapPath, nil, 0644)
			c.Assert(err, IsNil)
		} else {
			var err error
			kernelSnap, err = createMockSnapFile(c.MkDir(), snapPath, "kernel")
			c.Assert(err, IsNil)
		}

		mockBF = append(mockBF, bootloader.NewBootFile(snapPath, "kernel.efi", bootloader.RoleRecovery))

		myAuthKey := &ecdsa.PrivateKey{}

		myParams := secboot.SealKeysParams{
			ModelParams: []*secboot.SealKeyModelParams{
				{
					EFILoadChains: []*secboot.LoadChain{
						secboot.NewLoadChain(mockBF[0],
							secboot.NewLoadChain(mockBF[4])),
					},
					KernelCmdlines: []string{"cmdline1"},
					Model:          &asserts.Model{},
				},
				{
					EFILoadChains: []*secboot.LoadChain{
						secboot.NewLoadChain(mockBF[0],
							secboot.NewLoadChain(mockBF[2],
								secboot.NewLoadChain(mockBF[4])),
							secboot.NewLoadChain(mockBF[3],
								secboot.NewLoadChain(mockBF[4]))),
						secboot.NewLoadChain(mockBF[1],
							secboot.NewLoadChain(mockBF[2],
								secboot.NewLoadChain(mockBF[4])),
							secboot.NewLoadChain(mockBF[3],
								secboot.NewLoadChain(mockBF[4]))),
					},
					KernelCmdlines: []string{"cmdline2", "cmdline3"},
					Model:          &asserts.Model{},
				},
			},
			TPMPolicyAuthKey:       myAuthKey,
			TPMPolicyAuthKeyFile:   filepath.Join(tmpDir, "policy-auth-key-file"),
			TPMLockoutAuthFile:     filepath.Join(tmpDir, "lockout-auth-file"),
			TPMProvision:           !tc.skipProvision,
			PCRPolicyCounterHandle: 42,
		}

		myKey := secboot.EncryptionKey{}
		myKey2 := secboot.EncryptionKey{}
		for i := range myKey {
			myKey[i] = byte(i)
			myKey2[i] = byte(128 + i)
		}

		myKeys := []secboot.SealKeyRequest{
			{
				Key:     myKey,
				KeyFile: "keyfile",
			},
			{
				Key:     myKey2,
				KeyFile: "keyfile2",
			},
		}

		// events for
		// a -> kernel
		sequences1 := []*sb.EFIImageLoadEvent{
			{
				Source: sb.Firmware,
				Image:  sb.FileEFIImage(mockBF[0].Path),
				Next: []*sb.EFIImageLoadEvent{
					{
						Source: sb.Shim,
						Image: sb.SnapFileEFIImage{
							Container: kernelSnap,
							Path:      mockBF[4].Snap,
							FileName:  "kernel.efi",
						},
					},
				},
			},
		}

		// "cdk" events for
		// c -> kernel OR
		// d -> kernel
		cdk := []*sb.EFIImageLoadEvent{
			{
				Source: sb.Shim,
				Image:  sb.FileEFIImage(mockBF[2].Path),
				Next: []*sb.EFIImageLoadEvent{
					{
						Source: sb.Shim,
						Image: sb.SnapFileEFIImage{
							Container: kernelSnap,
							Path:      mockBF[4].Snap,
							FileName:  "kernel.efi",
						},
					},
				},
			},
			{
				Source: sb.Shim,
				Image:  sb.FileEFIImage(mockBF[3].Path),
				Next: []*sb.EFIImageLoadEvent{
					{
						Source: sb.Shim,
						Image: sb.SnapFileEFIImage{
							Container: kernelSnap,
							Path:      mockBF[4].Snap,
							FileName:  "kernel.efi",
						},
					},
				},
			},
		}

		// events for
		// a -> "cdk"
		// b -> "cdk"
		sequences2 := []*sb.EFIImageLoadEvent{
			{
				Source: sb.Firmware,
				Image:  sb.FileEFIImage(mockBF[0].Path),
				Next:   cdk,
			},
			{
				Source: sb.Firmware,
				Image:  sb.FileEFIImage(mockBF[1].Path),
				Next:   cdk,
			},
		}

		tpm, restore := mockSbTPMConnection(c, tc.tpmErr)
		defer restore()

		// mock adding EFI secure boot policy profile
		var pcrProfile *sb.PCRProtectionProfile
		addEFISbPolicyCalls := 0
		restore = secboot.MockSbAddEFISecureBootPolicyProfile(func(profile *sb.PCRProtectionProfile, params *sb.EFISecureBootPolicyProfileParams) error {
			addEFISbPolicyCalls++
			pcrProfile = profile
			c.Assert(params.PCRAlgorithm, Equals, tpm2.HashAlgorithmSHA256)
			switch addEFISbPolicyCalls {
			case 1:
				c.Assert(params.LoadSequences, DeepEquals, sequences1)
			case 2:
				c.Assert(params.LoadSequences, DeepEquals, sequences2)
			default:
				c.Error("AddEFISecureBootPolicyProfile shouldn't be called a third time")
			}
			return tc.addEFISbPolicyErr
		})
		defer restore()

		// mock adding EFI boot manager profile
		addEFIBootManagerCalls := 0
		restore = secboot.MockSbAddEFIBootManagerProfile(func(profile *sb.PCRProtectionProfile, params *sb.EFIBootManagerProfileParams) error {
			addEFIBootManagerCalls++
			c.Assert(profile, Equals, pcrProfile)
			c.Assert(params.PCRAlgorithm, Equals, tpm2.HashAlgorithmSHA256)
			switch addEFISbPolicyCalls {
			case 1:
				c.Assert(params.LoadSequences, DeepEquals, sequences1)
			case 2:
				c.Assert(params.LoadSequences, DeepEquals, sequences2)
			default:
				c.Error("AddEFIBootManagerProfile shouldn't be called a third time")
			}
			return tc.addEFIBootManagerErr
		})
		defer restore()

		// mock adding systemd EFI stub profile
		addSystemdEfiStubCalls := 0
		restore = secboot.MockSbAddSystemdEFIStubProfile(func(profile *sb.PCRProtectionProfile, params *sb.SystemdEFIStubProfileParams) error {
			addSystemdEfiStubCalls++
			c.Assert(profile, Equals, pcrProfile)
			c.Assert(params.PCRAlgorithm, Equals, tpm2.HashAlgorithmSHA256)
			c.Assert(params.PCRIndex, Equals, 12)
			switch addSystemdEfiStubCalls {
			case 1:
				c.Assert(params.KernelCmdlines, DeepEquals, myParams.ModelParams[0].KernelCmdlines)
			case 2:
				c.Assert(params.KernelCmdlines, DeepEquals, myParams.ModelParams[1].KernelCmdlines)
			default:
				c.Error("AddSystemdEFIStubProfile shouldn't be called a third time")
			}
			return tc.addSystemdEFIStubErr
		})
		defer restore()

		// mock adding snap model profile
		addSnapModelCalls := 0
		restore = secboot.MockSbAddSnapModelProfile(func(profile *sb.PCRProtectionProfile, params *sb.SnapModelProfileParams) error {
			addSnapModelCalls++
			c.Assert(profile, Equals, pcrProfile)
			c.Assert(params.PCRAlgorithm, Equals, tpm2.HashAlgorithmSHA256)
			c.Assert(params.PCRIndex, Equals, 12)
			switch addSnapModelCalls {
			case 1:
				c.Assert(params.Models[0], DeepEquals, myParams.ModelParams[0].Model)
			case 2:
				c.Assert(params.Models[0], DeepEquals, myParams.ModelParams[1].Model)
			default:
				c.Error("AddSnapModelProfile shouldn't be called a third time")
			}
			return tc.addSnapModelErr
		})
		defer restore()

		// mock provisioning
		provisioningCalls := 0
		restore = secboot.MockProvisionTPM(func(t *sb.TPMConnection, mode sb.ProvisionMode, newLockoutAuth []byte) error {
			provisioningCalls++
			c.Assert(t, Equals, tpm)
			c.Assert(mode, Equals, sb.ProvisionModeFull)
			c.Assert(myParams.TPMLockoutAuthFile, testutil.FilePresent)
			return tc.provisioningErr
		})
		defer restore()

		// mock sealing
		sealCalls := 0
		restore = secboot.MockSbSealKeyToTPMMultiple(func(t *sb.TPMConnection, kr []*sb.SealKeyRequest, params *sb.KeyCreationParams) (sb.TPMPolicyAuthKey, error) {
			sealCalls++
			c.Assert(t, Equals, tpm)
			c.Assert(kr, DeepEquals, []*sb.SealKeyRequest{{Key: myKey[:], Path: "keyfile"}, {Key: myKey2[:], Path: "keyfile2"}})
			c.Assert(params.AuthKey, Equals, myAuthKey)
			c.Assert(params.PCRPolicyCounterHandle, Equals, tpm2.Handle(42))
			return sb.TPMPolicyAuthKey{}, tc.sealErr
		})
		defer restore()

		// mock TPM enabled check
		restore = secboot.MockIsTPMEnabled(func(t *sb.TPMConnection) bool {
			return tc.tpmEnabled
		})
		defer restore()

		err := secboot.SealKeys(myKeys, &myParams)
		if tc.expectedErr == "" {
			c.Assert(err, IsNil)
			c.Assert(addEFISbPolicyCalls, Equals, 2)
			c.Assert(addSystemdEfiStubCalls, Equals, 2)
			c.Assert(addSnapModelCalls, Equals, 2)
			c.Assert(osutil.FileExists(myParams.TPMPolicyAuthKeyFile), Equals, true)
		} else {
			c.Assert(err, ErrorMatches, tc.expectedErr)
		}
		c.Assert(provisioningCalls, Equals, tc.provisioningCalls)
		c.Assert(sealCalls, Equals, tc.sealCalls)

	}
}

func (s *secbootSuite) TestResealKey(c *C) {
	mockErr := errors.New("some error")

	for _, tc := range []struct {
		tpmErr               error
		tpmEnabled           bool
		missingFile          bool
		addEFISbPolicyErr    error
		addEFIBootManagerErr error
		addSystemdEFIStubErr error
		addSnapModelErr      error
		provisioningErr      error
		resealErr            error
		resealCalls          int
		expectedErr          string
	}{
		{tpmErr: mockErr, expectedErr: "cannot connect to TPM: some error"},
		{tpmEnabled: false, expectedErr: "TPM device is not enabled"},
		{tpmEnabled: true, missingFile: true, expectedErr: "cannot build EFI image load sequences: file .*/file.efi does not exist"},
		{tpmEnabled: true, addEFISbPolicyErr: mockErr, expectedErr: "cannot add EFI secure boot policy profile: some error"},
		{tpmEnabled: true, addEFIBootManagerErr: mockErr, expectedErr: "cannot add EFI boot manager profile: some error"},
		{tpmEnabled: true, addSystemdEFIStubErr: mockErr, expectedErr: "cannot add systemd EFI stub profile: some error"},
		{tpmEnabled: true, addSnapModelErr: mockErr, expectedErr: "cannot add snap model profile: some error"},
		{tpmEnabled: true, resealErr: mockErr, resealCalls: 1, expectedErr: "some error"},
		{tpmEnabled: true, resealCalls: 1, expectedErr: ""},
	} {
		mockTPMPolicyAuthKey := []byte{1, 3, 3, 7}
		mockTPMPolicyAuthKeyFile := filepath.Join(c.MkDir(), "policy-auth-key-file")
		err := ioutil.WriteFile(mockTPMPolicyAuthKeyFile, mockTPMPolicyAuthKey, 0600)
		c.Assert(err, IsNil)

		mockEFI := bootloader.NewBootFile("", filepath.Join(c.MkDir(), "file.efi"), bootloader.RoleRecovery)
		if !tc.missingFile {
			err := ioutil.WriteFile(mockEFI.Path, nil, 0644)
			c.Assert(err, IsNil)
		}

		myParams := &secboot.ResealKeysParams{
			ModelParams: []*secboot.SealKeyModelParams{
				{
					EFILoadChains:  []*secboot.LoadChain{secboot.NewLoadChain(mockEFI)},
					KernelCmdlines: []string{"cmdline"},
					Model:          &asserts.Model{},
				},
			},
			KeyFiles:             []string{"keyfile", "keyfile2"},
			TPMPolicyAuthKeyFile: mockTPMPolicyAuthKeyFile,
		}

		sequences := []*sb.EFIImageLoadEvent{
			{
				Source: sb.Firmware,
				Image:  sb.FileEFIImage(mockEFI.Path),
			},
		}

		// mock TPM connection
		tpm, restore := mockSbTPMConnection(c, tc.tpmErr)
		defer restore()

		// mock TPM enabled check
		restore = secboot.MockIsTPMEnabled(func(t *sb.TPMConnection) bool {
			return tc.tpmEnabled
		})
		defer restore()

		// mock adding EFI secure boot policy profile
		var pcrProfile *sb.PCRProtectionProfile
		addEFISbPolicyCalls := 0
		restore = secboot.MockSbAddEFISecureBootPolicyProfile(func(profile *sb.PCRProtectionProfile, params *sb.EFISecureBootPolicyProfileParams) error {
			addEFISbPolicyCalls++
			pcrProfile = profile
			c.Assert(params.PCRAlgorithm, Equals, tpm2.HashAlgorithmSHA256)
			c.Assert(params.LoadSequences, DeepEquals, sequences)
			return tc.addEFISbPolicyErr
		})
		defer restore()

		// mock adding EFI boot manager profile
		addEFIBootManagerCalls := 0
		restore = secboot.MockSbAddEFIBootManagerProfile(func(profile *sb.PCRProtectionProfile, params *sb.EFIBootManagerProfileParams) error {
			addEFIBootManagerCalls++
			c.Assert(profile, Equals, pcrProfile)
			c.Assert(params.PCRAlgorithm, Equals, tpm2.HashAlgorithmSHA256)
			c.Assert(params.LoadSequences, DeepEquals, sequences)
			return tc.addEFIBootManagerErr
		})
		defer restore()

		// mock adding systemd EFI stub profile
		addSystemdEfiStubCalls := 0
		restore = secboot.MockSbAddSystemdEFIStubProfile(func(profile *sb.PCRProtectionProfile, params *sb.SystemdEFIStubProfileParams) error {
			addSystemdEfiStubCalls++
			c.Assert(profile, Equals, pcrProfile)
			c.Assert(params.PCRAlgorithm, Equals, tpm2.HashAlgorithmSHA256)
			c.Assert(params.PCRIndex, Equals, 12)
			c.Assert(params.KernelCmdlines, DeepEquals, myParams.ModelParams[0].KernelCmdlines)
			return tc.addSystemdEFIStubErr
		})
		defer restore()

		// mock adding snap model profile
		addSnapModelCalls := 0
		restore = secboot.MockSbAddSnapModelProfile(func(profile *sb.PCRProtectionProfile, params *sb.SnapModelProfileParams) error {
			addSnapModelCalls++
			c.Assert(profile, Equals, pcrProfile)
			c.Assert(params.PCRAlgorithm, Equals, tpm2.HashAlgorithmSHA256)
			c.Assert(params.PCRIndex, Equals, 12)
			c.Assert(params.Models[0], DeepEquals, myParams.ModelParams[0].Model)
			return tc.addSnapModelErr
		})
		defer restore()

		// mock PCR protection policy update
		resealCalls := 0
		restore = secboot.MockSbUpdateKeyPCRProtectionPolicyMultiple(func(t *sb.TPMConnection, keyPaths []string, authKey sb.TPMPolicyAuthKey, profile *sb.PCRProtectionProfile) error {
			resealCalls++
			c.Assert(t, Equals, tpm)
			c.Assert(keyPaths, DeepEquals, []string{"keyfile", "keyfile2"})
			c.Assert(authKey, DeepEquals, sb.TPMPolicyAuthKey(mockTPMPolicyAuthKey))
			c.Assert(profile, Equals, pcrProfile)
			return tc.resealErr
		})
		defer restore()

		err = secboot.ResealKeys(myParams)
		if tc.expectedErr == "" {
			c.Assert(err, IsNil)
			c.Assert(addEFISbPolicyCalls, Equals, 1)
			c.Assert(addSystemdEfiStubCalls, Equals, 1)
			c.Assert(addSnapModelCalls, Equals, 1)
		} else {
			c.Assert(err, ErrorMatches, tc.expectedErr)
		}
		c.Assert(resealCalls, Equals, tc.resealCalls)
	}
}

func (s *secbootSuite) TestSealKeyNoModelParams(c *C) {
	myKeys := []secboot.SealKeyRequest{
		{
			Key:     secboot.EncryptionKey{},
			KeyFile: "keyfile",
		},
	}
	myParams := secboot.SealKeysParams{
		TPMPolicyAuthKeyFile: "policy-auth-key-file",
		TPMLockoutAuthFile:   "lockout-auth-file",
	}

	err := secboot.SealKeys(myKeys, &myParams)
	c.Assert(err, ErrorMatches, "at least one set of model-specific parameters is required")
}

func createMockSnapFile(snapDir, snapPath, snapType string) (snap.Container, error) {
	snapYamlPath := filepath.Join(snapDir, "meta/snap.yaml")
	if err := os.MkdirAll(filepath.Dir(snapYamlPath), 0755); err != nil {
		return nil, err
	}
	if err := ioutil.WriteFile(snapYamlPath, []byte("name: foo"), 0644); err != nil {
		return nil, err
	}
	sqfs := squashfs.New(snapPath)
	if err := sqfs.Build(snapDir, &squashfs.BuildOpts{SnapType: snapType}); err != nil {
		return nil, err
	}
	return snapfile.Open(snapPath)
}

func mockSbTPMConnection(c *C, tpmErr error) (*sb.TPMConnection, func()) {
	tcti, err := os.Open("/dev/null")
	c.Assert(err, IsNil)
	tpmctx, err := tpm2.NewTPMContext(tcti)
	c.Assert(err, IsNil)
	tpm := &sb.TPMConnection{TPMContext: tpmctx}
	restore := secboot.MockSbConnectToDefaultTPM(func() (*sb.TPMConnection, error) {
		if tpmErr != nil {
			return nil, tpmErr
		}
		return tpm, nil
	})
	return tpm, restore
}

func (s *secbootSuite) TestUnlockEncryptedVolumeUsingKeyBadDisk(c *C) {
	disk := &disks.MockDiskMapping{
		FilesystemLabelToPartUUID: map[string]string{},
	}
	dev, err := secboot.UnlockEncryptedVolumeUsingKey(disk, "ubuntu-save", []byte("fooo"))
	c.Assert(err, ErrorMatches, `filesystem label "ubuntu-save-enc" not found`)
	c.Check(dev, Equals, "")
}

func (s *secbootSuite) TestUnlockEncryptedVolumeUsingKeyHappy(c *C) {
	disk := &disks.MockDiskMapping{
		FilesystemLabelToPartUUID: map[string]string{
			"ubuntu-save-enc": "123-123-123",
		},
	}
	restore := secboot.MockRandomKernelUUID(func() string {
		return "random-uuid-123-123"
	})
	defer restore()
	restore = secboot.MockSbActivateVolumeWithKey(func(volumeName, sourceDevicePath string, key []byte,
		options *sb.ActivateVolumeOptions) error {
		c.Check(options, DeepEquals, &sb.ActivateVolumeOptions{})
		c.Check(key, DeepEquals, []byte("fooo"))
		c.Check(volumeName, Matches, "ubuntu-save-random-uuid-123-123")
		c.Check(sourceDevicePath, Equals, "/dev/disk/by-partuuid/123-123-123")
		return nil
	})
	defer restore()
	dev, err := secboot.UnlockEncryptedVolumeUsingKey(disk, "ubuntu-save", []byte("fooo"))
	c.Assert(err, IsNil)
	c.Check(dev, Equals, "/dev/mapper/ubuntu-save-random-uuid-123-123")
}

func (s *secbootSuite) TestUnlockEncryptedVolumeUsingKeyErr(c *C) {
	disk := &disks.MockDiskMapping{
		FilesystemLabelToPartUUID: map[string]string{
			"ubuntu-save-enc": "123-123-123",
		},
	}
	restore := secboot.MockRandomKernelUUID(func() string {
		return "random-uuid-123-123"
	})
	defer restore()
	restore = secboot.MockSbActivateVolumeWithKey(func(volumeName, sourceDevicePath string, key []byte,
		options *sb.ActivateVolumeOptions) error {
		return fmt.Errorf("failed")
	})
	defer restore()
	dev, err := secboot.UnlockEncryptedVolumeUsingKey(disk, "ubuntu-save", []byte("fooo"))
	c.Assert(err, ErrorMatches, "failed")
	c.Check(dev, Equals, "")
}
