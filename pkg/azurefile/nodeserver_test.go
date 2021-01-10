/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azurefile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"sigs.k8s.io/azurefile-csi-driver/test/utils/testutil"

	azure2 "github.com/Azure/go-autorest/autorest/azure"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"
	"k8s.io/utils/mount"

	azure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

const (
	sourceTest = "./source_test"
	targetTest = "./target_test"
)

type ExecArgs struct {
	command string
	args    []string
	output  string
	err     error
}

func matchFlakyWindowsError(mainError error, substr string) bool {
	var errorMessage string
	if mainError == nil {
		errorMessage = ""
	} else {
		errorMessage = mainError.Error()
	}

	return strings.Contains(errorMessage, substr)
}

func TestNodeGetInfo(t *testing.T) {
	d := NewFakeDriver()

	// Test valid request
	req := csi.NodeGetInfoRequest{}
	resp, err := d.NodeGetInfo(context.Background(), &req)
	assert.NoError(t, err)
	assert.Equal(t, resp.GetNodeId(), fakeNodeID)
}

func TestNodeGetCapabilities(t *testing.T) {
	d := NewFakeDriver()
	capType := &csi.NodeServiceCapability_Rpc{
		Rpc: &csi.NodeServiceCapability_RPC{
			Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		},
	}
	capList := []*csi.NodeServiceCapability{{
		Type: capType,
	}}
	d.NSCap = capList
	// Test valid request
	req := csi.NodeGetCapabilitiesRequest{}
	resp, err := d.NodeGetCapabilities(context.Background(), &req)
	assert.NotNil(t, resp)
	assert.Equal(t, resp.Capabilities[0].GetType(), capType)
	assert.NoError(t, err)
}

func TestNodePublishVolume(t *testing.T) {
	d := NewFakeDriver()
	volumeCap := csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}
	var (
		errorMountSource     = testutil.GetWorkDirPath("error_mount_source", t)
		alreadyMountedTarget = testutil.GetWorkDirPath("false_is_likely_exist_target", t)
		azureFile            = testutil.GetWorkDirPath("azure.go", t)

		sourceTest = testutil.GetWorkDirPath("source_test", t)
		targetTest = testutil.GetWorkDirPath("target_test", t)
	)

	tests := []struct {
		desc        string
		setup       func()
		req         csi.NodePublishVolumeRequest
		expectedErr testutil.TestError
		cleanup     func()
	}{
		{
			desc: "[Error] Volume capabilities missing",
			req:  csi.NodePublishVolumeRequest{},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Volume capability missing in request"),
			},
		},
		{
			desc: "[Error] Volume ID missing",
			req:  csi.NodePublishVolumeRequest{VolumeCapability: &csi.VolumeCapability{AccessMode: &volumeCap}},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Volume ID missing in request"),
			},
		},
		{
			desc: "[Error] Target path missing",
			req: csi.NodePublishVolumeRequest{VolumeCapability: &csi.VolumeCapability{AccessMode: &volumeCap},
				VolumeId: "vol_1"},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Target path not provided"),
			},
		},
		{
			desc: "[Error] Stage target path missing",
			req: csi.NodePublishVolumeRequest{VolumeCapability: &csi.VolumeCapability{AccessMode: &volumeCap},
				VolumeId:   "vol_1",
				TargetPath: targetTest},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Staging target not provided"),
			},
		},
		{
			desc: "[Error] Volume operation in progress",
			setup: func() {
				d.volumeLocks.TryAcquire("vol_1")
			},
			req: csi.NodePublishVolumeRequest{VolumeCapability: &csi.VolumeCapability{AccessMode: &volumeCap},
				VolumeId:          "vol_1",
				TargetPath:        targetTest,
				StagingTargetPath: sourceTest,
				Readonly:          true},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.Aborted, fmt.Sprintf(volumeOperationAlreadyExistsFmt, "vol_1")),
			},
			cleanup: func() {
				d.volumeLocks.Release("vol_1")
			},
		},
		{
			desc: "[Error] Not a directory",
			req: csi.NodePublishVolumeRequest{VolumeCapability: &csi.VolumeCapability{AccessMode: &volumeCap},
				VolumeId:          "vol_1",
				TargetPath:        azureFile,
				StagingTargetPath: sourceTest,
				Readonly:          true},
			expectedErr: testutil.TestError{
				DefaultError: status.Errorf(codes.Internal, fmt.Sprintf("Could not mount target \"%s\": mkdir %s: not a directory", azureFile, azureFile)),
				WindowsError: status.Errorf(codes.Internal, fmt.Sprintf("Could not mount target %#v: mkdir %s: The system cannot find the path specified.", azureFile, azureFile)),
			},
		},
		{
			desc: "[Error] Mount error mocked by Mount",
			req: csi.NodePublishVolumeRequest{VolumeCapability: &csi.VolumeCapability{AccessMode: &volumeCap},
				VolumeId:          "vol_1",
				TargetPath:        targetTest,
				StagingTargetPath: errorMountSource,
				Readonly:          true},
			expectedErr: testutil.TestError{
				DefaultError: status.Errorf(codes.Internal, fmt.Sprintf("Could not mount \"%s\" at \"%s\": fake Mount: source error", errorMountSource, targetTest)),
			},
		},
		{
			desc: "[Success] Valid request read only",
			req: csi.NodePublishVolumeRequest{VolumeCapability: &csi.VolumeCapability{AccessMode: &volumeCap},
				VolumeId:          "vol_1",
				TargetPath:        targetTest,
				StagingTargetPath: sourceTest,
				Readonly:          true},
			expectedErr: testutil.TestError{},
		},
		{
			desc: "[Success] Valid request already mounted",
			req: csi.NodePublishVolumeRequest{VolumeCapability: &csi.VolumeCapability{AccessMode: &volumeCap},
				VolumeId:          "vol_1",
				TargetPath:        alreadyMountedTarget,
				StagingTargetPath: sourceTest,
				Readonly:          true},
			expectedErr: testutil.TestError{},
		},
		{
			desc: "[Success] Valid request",
			req: csi.NodePublishVolumeRequest{VolumeCapability: &csi.VolumeCapability{AccessMode: &volumeCap},
				VolumeId:          "vol_1",
				TargetPath:        targetTest,
				StagingTargetPath: sourceTest,
				Readonly:          true},
			expectedErr: testutil.TestError{},
		},
	}

	// Setup
	_ = makeDir(alreadyMountedTarget)
	mounter, err := NewFakeMounter()
	if err != nil {
		t.Fatalf(fmt.Sprintf("failed to get fake mounter: %v", err))
	}
	if runtime.GOOS != "windows" {
		mounter.Exec = &testingexec.FakeExec{ExactOrder: true}
	}
	d.mounter = mounter

	for _, test := range tests {
		if test.setup != nil {
			test.setup()
		}
		_, err := d.NodePublishVolume(context.Background(), &test.req)
		if !testutil.AssertError(err, &test.expectedErr) {
			t.Errorf("test case: %s, \nUnexpected error: %v\nExpected error: %v", test.desc, err, test.expectedErr.GetExpectedError())
		}
		if test.cleanup != nil {
			test.cleanup()
		}
	}

	// Clean up
	err = os.RemoveAll(targetTest)
	assert.NoError(t, err)
	err = os.RemoveAll(alreadyMountedTarget)
	assert.NoError(t, err)
}

func TestNodeUnpublishVolume(t *testing.T) {
	errorTarget := testutil.GetWorkDirPath("error_is_likely_target", t)
	targetFile := testutil.GetWorkDirPath("abc.go", t)
	d := NewFakeDriver()

	tests := []struct {
		desc         string
		setup        func()
		req          csi.NodeUnpublishVolumeRequest
		skipOnDarwin bool
		expectedErr  testutil.TestError
		cleanup      func()
	}{
		{
			desc: "[Error] Volume ID missing",
			req:  csi.NodeUnpublishVolumeRequest{TargetPath: targetTest},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Volume ID missing in request"),
			},
		},
		{
			desc: "[Error] Target missing",
			req:  csi.NodeUnpublishVolumeRequest{VolumeId: "vol_1"},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Target path missing in request"),
			},
		},
		{
			desc: "[Error] Volume operation in progress",
			setup: func() {
				d.volumeLocks.TryAcquire("vol_1")
			},
			req: csi.NodeUnpublishVolumeRequest{TargetPath: targetFile, VolumeId: "vol_1"},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.Aborted, fmt.Sprintf(volumeOperationAlreadyExistsFmt, "vol_1")),
			},
			cleanup: func() {
				d.volumeLocks.Release("vol_1")
			},
		},
		{
			desc:         "[Error] Unmount error mocked by IsLikelyNotMountPoint",
			skipOnDarwin: true,
			req:          csi.NodeUnpublishVolumeRequest{TargetPath: errorTarget, VolumeId: "vol_1"},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.Internal, fmt.Sprintf("failed to unmount target \"%s\": fake IsLikelyNotMountPoint: fake error", errorTarget)),
			},
		},
		{
			desc:        "[Success] Valid request",
			req:         csi.NodeUnpublishVolumeRequest{TargetPath: targetFile, VolumeId: "vol_1"},
			expectedErr: testutil.TestError{},
		},
	}

	// Setup
	_ = makeDir(errorTarget)
	mounter, err := NewFakeMounter()
	if err != nil {
		t.Fatalf(fmt.Sprintf("failed to get fake mounter: %v", err))
	}
	if runtime.GOOS != "windows" {
		mounter.Exec = &testingexec.FakeExec{ExactOrder: true}
	}
	d.mounter = mounter

	for _, test := range tests {
		if test.setup != nil {
			test.setup()
		}
		if test.skipOnDarwin && runtime.GOOS == "darwin" {
			continue
		}
		_, err := d.NodeUnpublishVolume(context.Background(), &test.req)
		if !testutil.AssertError(err, &test.expectedErr) {
			t.Errorf("test case: %s, \nUnexpected error: %v\nExpected error: %v", test.desc, err, test.expectedErr.GetExpectedError())
		}
		if test.cleanup != nil {
			test.cleanup()
		}
	}

	// Clean up
	err = os.RemoveAll(errorTarget)
	assert.NoError(t, err)
}

func TestNodeStageVolume(t *testing.T) {
	stdVolCap := csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{},
		},
	}
	d := NewFakeDriver()

	var (
		errorMountSensSource   = testutil.GetWorkDirPath("error_mount_sens_source", t)
		sourceTest             = testutil.GetWorkDirPath("source_test", t)
		azureStagingTargetPath = testutil.GetWorkDirPath("azure.go", t)
		proxyMountPath         = testutil.GetWorkDirPath("proxy-mount", t)
		testDiskPath           = fmt.Sprintf("%s/test_disk", proxyMountPath)
	)

	volContextEmptyDiskName := map[string]string{
		fsTypeField:     "ext4",
		diskNameField:   "",
		shareNameField:  "test_sharename",
		serverNameField: "test_servername",
	}
	volContextEmptyShareName := map[string]string{
		fsTypeField:     "test_field",
		diskNameField:   "test_disk",
		shareNameField:  "test_sharename",
		serverNameField: "",
	}
	volContextNfs := map[string]string{
		fsTypeField:     "nfs",
		diskNameField:   "test_disk",
		shareNameField:  "test_sharename",
		serverNameField: "test_servername",
	}
	volContext := map[string]string{
		fsTypeField:     "test_field",
		diskNameField:   "test_disk",
		shareNameField:  "test_sharename",
		serverNameField: "test_servername",
	}
	volContextFsType := map[string]string{
		fsTypeField:     "ext4",
		diskNameField:   "test_disk",
		shareNameField:  "test_sharename",
		serverNameField: "test_servername",
	}
	errorSource := `\\\\test_servername\\test_sharename`

	secrets := map[string]string{
		"accountname": "k8s",
		"accountkey":  "testkey",
	}

	tests := []struct {
		desc         string
		setup        func()
		req          csi.NodeStageVolumeRequest
		execScripts  []ExecArgs
		skipOnDarwin bool
		expectedErr  testutil.TestError
		// use this field only when Windows
		// gives flaky error messages due
		// to CSI proxy
		// This field holds the base error message
		// that is common amongst all other flaky
		// error messages
		flakyWindowsErrorMessage string
		cleanup                  func()
	}{
		{
			desc:        "[Error] Volume ID missing",
			req:         csi.NodeStageVolumeRequest{},
			execScripts: nil,
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Volume ID missing in request"),
			},
		},
		{
			desc: "[Error] Stage target path missing",
			req:  csi.NodeStageVolumeRequest{VolumeId: "vol_1"},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Staging target not provided"),
			},
		},
		{
			desc: "[Error] Volume capabilities missing",
			req:  csi.NodeStageVolumeRequest{VolumeId: "vol_1", StagingTargetPath: sourceTest},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Volume capability not provided"),
			},
		},
		{
			desc: "[Error] GetAccountInfo error parsing volume id",
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1", StagingTargetPath: sourceTest,
				VolumeCapability: &stdVolCap},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "failed to get account name from vol_1"),
			},
		},
		{
			desc: "[Error] Volume operation in progress",
			setup: func() {
				d.volumeLocks.TryAcquire("vol_1##")
			},
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1##", StagingTargetPath: sourceTest,
				VolumeCapability: &stdVolCap,
				VolumeContext:    volContext,
				Secrets:          secrets},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.Aborted, fmt.Sprintf(volumeOperationAlreadyExistsFmt, "vol_1##")),
			},
			cleanup: func() {
				d.volumeLocks.Release("vol_1##")
			},
		},
		{
			desc: "[Error] Not a Directory",
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1##", StagingTargetPath: azureStagingTargetPath,
				VolumeCapability: &stdVolCap,
				VolumeContext:    volContext,
				Secrets:          secrets},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.Internal, fmt.Sprintf("MkdirAll %s failed with error: mkdir %s: not a directory", azureStagingTargetPath, azureStagingTargetPath)),
				WindowsError: status.Error(codes.Internal, fmt.Sprintf("Could not mount target %#v: mkdir %s: The system cannot find the path specified.", azureStagingTargetPath, azureStagingTargetPath)),
			},
		},
		{
			desc: "[Error] Empty Disk Name",
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1##", StagingTargetPath: sourceTest,
				VolumeCapability: &stdVolCap,
				VolumeContext:    volContextEmptyDiskName,
				Secrets:          secrets},
			expectedErr: testutil.TestError{
				DefaultError: status.Errorf(codes.Internal, fmt.Sprintf("diskname could not be empty, targetPath: %s", sourceTest)),
			},
		},
		{
			desc: "[Error] Failed SMB mount mocked by MountSensitive",
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1##", StagingTargetPath: errorMountSensSource,
				VolumeCapability: &stdVolCap,
				VolumeContext:    volContext,
				Secrets:          secrets},
			skipOnDarwin: true,
			flakyWindowsErrorMessage: fmt.Sprintf("volume(vol_1##) mount \"%s\" on %#v failed "+
				"with smb mapping failed with error: rpc error: code = Unknown desc = NewSmbGlobalMapping failed.",
				errorSource, errorMountSensSource),
			expectedErr: testutil.TestError{
				DefaultError: status.Errorf(codes.Internal, fmt.Sprintf("volume(vol_1##) mount \"//test_servername/test_sharename\" on %#v failed with fake MountSensitive: target error", errorMountSensSource)),
			},
		},
		{
			desc: "[Error] FormatAndMount mocked by exec commands",
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1##", StagingTargetPath: sourceTest,
				VolumeCapability: &stdVolCap,
				VolumeContext:    volContextFsType,
				Secrets:          secrets},
			execScripts: []ExecArgs{
				{"blkid", []string{"-p", "-s", "TYPE", "-s", "PTTYPE", "-o", "export", testDiskPath}, "", &testingexec.FakeExitError{Status: 2}},
				{"mkfs.ext4", []string{"-F", "-m0", testDiskPath}, "", fmt.Errorf("formatting failed")},
			},
			skipOnDarwin: true,
			flakyWindowsErrorMessage: fmt.Sprintf("volume(vol_1##) mount \"%s\" on %#v failed with "+
				"smb mapping failed with error: rpc error: code = Unknown desc = NewSmbGlobalMapping failed.",
				errorSource, proxyMountPath),
			expectedErr: testutil.TestError{
				DefaultError: status.Errorf(codes.Internal, "could not format %#v and mount it at %#v", sourceTest, testDiskPath),
			},
		},
		{
			desc: "[Success] Valid request",
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1##", StagingTargetPath: sourceTest,
				VolumeCapability: &stdVolCap,
				VolumeContext:    volContext,
				Secrets:          secrets},
			flakyWindowsErrorMessage: fmt.Sprintf("volume(vol_1##) mount \"%s\" on %#v failed with "+
				"smb mapping failed with error: rpc error: code = Unknown desc = NewSmbGlobalMapping failed.",
				errorSource, sourceTest),
			expectedErr: testutil.TestError{},
		},
		{
			desc: "[Success] Valid request with share name empty",
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1##", StagingTargetPath: sourceTest,
				VolumeCapability: &stdVolCap,
				VolumeContext:    volContextEmptyShareName,
				Secrets:          secrets},
			flakyWindowsErrorMessage: fmt.Sprintf("volume(vol_1##) mount \"\\\\\\\\k8s.file.test_suffix\\\\test_sharename\" on %#v failed with "+
				"smb mapping failed with error: rpc error: code = Unknown desc = NewSmbGlobalMapping failed.",
				sourceTest),
			expectedErr: testutil.TestError{},
		},
		{
			desc: "[Success] Valid request with fsType as nfs",
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1##", StagingTargetPath: sourceTest,
				VolumeCapability: &stdVolCap,
				VolumeContext:    volContextNfs,
				Secrets:          secrets},
			flakyWindowsErrorMessage: fmt.Sprintf("volume(vol_1##) mount \"%s\" on %#v failed with "+
				"smb mapping failed with error: rpc error: code = Unknown desc = NewSmbGlobalMapping failed.",
				errorSource, sourceTest),
			expectedErr: testutil.TestError{},
		},
		{
			desc: "[Success] Valid request with supported fsType disk",
			req: csi.NodeStageVolumeRequest{VolumeId: "vol_1##", StagingTargetPath: sourceTest,
				VolumeCapability: &stdVolCap,
				VolumeContext:    volContextFsType,
				Secrets:          secrets},
			execScripts: []ExecArgs{
				{"blkid", []string{"-p", "-s", "TYPE", "-s", "PTTYPE", "-o", "export", testDiskPath}, "", nil},
				{"mkfs.ext4", []string{"-F", "-m0", testDiskPath}, "", nil},
			},
			flakyWindowsErrorMessage: fmt.Sprintf("volume(vol_1##) mount \"%s\" on %#v failed with "+
				"smb mapping failed with error: rpc error: code = Unknown desc = NewSmbGlobalMapping failed.",
				errorSource, proxyMountPath),
			expectedErr: testutil.TestError{},
		},
	}

	// Setup
	for _, test := range tests {
		if test.setup != nil {
			test.setup()
		}
		if test.skipOnDarwin && runtime.GOOS == "darwin" {
			continue
		}
		mounter, err := NewFakeMounter()
		if err != nil {
			t.Fatalf(fmt.Sprintf("failed to get fake mounter: %v", err))
		}

		if runtime.GOOS != "windows" {
			fakeExec := &testingexec.FakeExec{ExactOrder: true}
			for _, script := range test.execScripts {
				fakeCmd := &testingexec.FakeCmd{}
				cmdAction := makeFakeCmd(fakeCmd, script.command, script.args...)
				outputAction := makeFakeOutput(script.output, script.err)
				fakeCmd.CombinedOutputScript = append(fakeCmd.CombinedOutputScript, outputAction)
				fakeExec.CommandScript = append(fakeExec.CommandScript, cmdAction)
			}
			mounter.Exec = fakeExec
		}

		d.mounter = mounter
		d.cloud = &azure.Cloud{
			Environment: azure2.Environment{StorageEndpointSuffix: "test_suffix"},
		}

		_, err = d.NodeStageVolume(context.Background(), &test.req)
		// separate assertion for flaky error messages
		if test.flakyWindowsErrorMessage != "" && runtime.GOOS == "windows" {
			if !matchFlakyWindowsError(err, test.flakyWindowsErrorMessage) {
				t.Errorf("test case: %s, \nUnexpected error: %v\nExpected error: %v", test.desc, err, test.flakyWindowsErrorMessage)
			}
		} else {
			if !testutil.AssertError(err, &test.expectedErr) {
				t.Errorf("test case: %s, \nUnexpected error: %v\nExpected error: %v", test.desc, err, test.expectedErr.GetExpectedError())
			}
		}
		if test.cleanup != nil {
			test.cleanup()
		}
	}

	// Clean up
	err := os.RemoveAll(sourceTest)
	assert.NoError(t, err)
	err = os.RemoveAll(proxyMount)
	assert.NoError(t, err)
	err = os.RemoveAll(errorMountSensSource)
	assert.NoError(t, err)
}

func TestNodeUnstageVolume(t *testing.T) {
	var (
		errorTarget = testutil.GetWorkDirPath("error_is_likely_target", t)
		targetFile  = testutil.GetWorkDirPath("abc.go", t)
	)
	d := NewFakeDriver()

	tests := []struct {
		desc         string
		setup        func()
		req          csi.NodeUnstageVolumeRequest
		skipOnDarwin bool
		expectedErr  testutil.TestError
		cleanup      func()
	}{
		{
			desc: "[Error] Volume ID missing",
			req:  csi.NodeUnstageVolumeRequest{StagingTargetPath: targetTest},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Volume ID missing in request"),
			},
		},
		{
			desc: "[Error] Target missing",
			req:  csi.NodeUnstageVolumeRequest{VolumeId: "vol_1"},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.InvalidArgument, "Staging target not provided"),
			},
		},
		{
			desc: "[Error] Volume operation in progress",
			setup: func() {
				d.volumeLocks.TryAcquire("vol_1")
			},
			req: csi.NodeUnstageVolumeRequest{StagingTargetPath: targetFile, VolumeId: "vol_1"},
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.Aborted, fmt.Sprintf(volumeOperationAlreadyExistsFmt, "vol_1")),
			},
			cleanup: func() {
				d.volumeLocks.Release("vol_1")
			},
		},
		{
			desc:         "[Error] CleanupMountPoint error mocked by IsLikelyNotMountPoint",
			req:          csi.NodeUnstageVolumeRequest{StagingTargetPath: errorTarget, VolumeId: "vol_1"},
			skipOnDarwin: true,
			expectedErr: testutil.TestError{
				DefaultError: status.Error(codes.Internal, fmt.Sprintf("failed to unmount staging target %#v: fake IsLikelyNotMountPoint: fake error", errorTarget)),
			},
		},
		{
			desc:        "[Success] Valid request",
			req:         csi.NodeUnstageVolumeRequest{StagingTargetPath: targetFile, VolumeId: "vol_1"},
			expectedErr: testutil.TestError{},
		},
	}

	// Setup
	_ = makeDir(errorTarget)
	mounter, err := NewFakeMounter()
	if err != nil {
		t.Fatalf(fmt.Sprintf("failed to get fake mounter: %v", err))
	}
	if runtime.GOOS != "windows" {
		mounter.Exec = &testingexec.FakeExec{ExactOrder: true}
	}
	d.mounter = mounter

	for _, test := range tests {
		if test.setup != nil {
			test.setup()
		}
		if test.skipOnDarwin && runtime.GOOS == "darwin" {
			continue
		}
		_, err := d.NodeUnstageVolume(context.Background(), &test.req)
		if !testutil.AssertError(err, &test.expectedErr) {
			t.Errorf("Desc: %v\nUnexcpected error: %v\nExpected: %v", test.desc, err, test.expectedErr.GetExpectedError())
		}
		if test.cleanup != nil {
			test.cleanup()
		}
	}

	// Clean up
	err = os.RemoveAll(errorTarget)
	assert.NoError(t, err)
}

func TestNodeGetVolumeStats(t *testing.T) {
	nonexistedPath := "/not/a/real/directory"
	fakePath := "/tmp/fake-volume-path"

	tests := []struct {
		desc        string
		req         csi.NodeGetVolumeStatsRequest
		expectedErr error
	}{
		{
			desc:        "[Error] Volume ID missing",
			req:         csi.NodeGetVolumeStatsRequest{VolumePath: targetTest},
			expectedErr: status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume ID was empty"),
		},
		{
			desc:        "[Error] VolumePath missing",
			req:         csi.NodeGetVolumeStatsRequest{VolumeId: "vol_1"},
			expectedErr: status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume path was empty"),
		},
		{
			desc:        "[Error] Incorrect volume path",
			req:         csi.NodeGetVolumeStatsRequest{VolumePath: nonexistedPath, VolumeId: "vol_1"},
			expectedErr: status.Errorf(codes.NotFound, "path /not/a/real/directory does not exist"),
		},
		{
			desc:        "[Success] Standard success",
			req:         csi.NodeGetVolumeStatsRequest{VolumePath: fakePath, VolumeId: "vol_1"},
			expectedErr: nil,
		},
	}

	// Setup
	_ = makeDir(fakePath)
	d := NewFakeDriver()

	for _, test := range tests {
		_, err := d.NodeGetVolumeStats(context.Background(), &test.req)
		//t.Errorf("[debug] error: %v\n metrics: %v", err, metrics)
		if !reflect.DeepEqual(err, test.expectedErr) {
			t.Errorf("desc: %v, expected error: %v, actual error: %v", test.desc, test.expectedErr, err)
		}
	}

	// Clean up
	err := os.RemoveAll(fakePath)
	assert.NoError(t, err)
}

func TestEnsureMountPoint(t *testing.T) {
	errorTarget := "./error_is_likely_target"
	alreadyExistTarget := "./false_is_likely_exist_target"
	falseTarget := "./false_is_likely_target"
	azureFile := "./azure.go"

	tests := []struct {
		desc        string
		target      string
		expectedErr error
	}{
		{
			desc:        "[Error] Mocked by IsLikelyNotMountPoint",
			target:      errorTarget,
			expectedErr: fmt.Errorf("fake IsLikelyNotMountPoint: fake error"),
		},
		{
			desc:        "[Error] Error opening file",
			target:      falseTarget,
			expectedErr: &os.PathError{Op: "open", Path: "./false_is_likely_target", Err: syscall.ENOENT},
		},
		{
			desc:        "[Error] Not a directory",
			target:      azureFile,
			expectedErr: &os.PathError{Op: "mkdir", Path: "./azure.go", Err: syscall.ENOTDIR},
		},
		{
			desc:        "[Success] Successful run",
			target:      targetTest,
			expectedErr: nil,
		},
		{
			desc:        "[Success] Already existing mount",
			target:      alreadyExistTarget,
			expectedErr: nil,
		},
	}

	// Setup
	_ = makeDir(alreadyExistTarget)
	d := NewFakeDriver()
	fakeMounter := &fakeMounter{}
	fakeExec := &testingexec.FakeExec{ExactOrder: true}
	d.mounter = &mount.SafeFormatAndMount{
		Interface: fakeMounter,
		Exec:      fakeExec,
	}

	for _, test := range tests {
		_, err := d.ensureMountPoint(test.target)
		if !reflect.DeepEqual(err, test.expectedErr) {
			t.Errorf("[%s]: Unexpected Error: %v, expected error: %v", test.desc, err, test.expectedErr)
		}
	}

	// Clean up
	err := os.RemoveAll(alreadyExistTarget)
	assert.NoError(t, err)
	err = os.RemoveAll(targetTest)
	assert.NoError(t, err)
}

func TestMakeDir(t *testing.T) {
	//Successfully create directory
	err := makeDir(targetTest)
	assert.NoError(t, err)

	//Failed case
	err = makeDir("./azure.go")
	var e *os.PathError
	if !errors.As(err, &e) {
		t.Errorf("Unexpected Error: %v", err)
	}

	// Remove the directory created
	err = os.RemoveAll(targetTest)
	assert.NoError(t, err)
}

func TestNodeExpandVolume(t *testing.T) {
	d := NewFakeDriver()
	req := csi.NodeExpandVolumeRequest{}
	resp, err := d.NodeExpandVolume(context.Background(), &req)
	assert.Nil(t, resp)
	if !reflect.DeepEqual(err, status.Error(codes.Unimplemented, "")) {
		t.Errorf("Unexpected error: %v", err)
	}
}

func makeFakeCmd(fakeCmd *testingexec.FakeCmd, cmd string, args ...string) testingexec.FakeCommandAction {
	c := cmd
	a := args
	return func(cmd string, args ...string) exec.Cmd {
		command := testingexec.InitFakeCmd(fakeCmd, c, a...)
		return command
	}
}

func makeFakeOutput(output string, err error) testingexec.FakeAction {
	o := output
	return func() ([]byte, []byte, error) {
		return []byte(o), nil, err
	}
}
