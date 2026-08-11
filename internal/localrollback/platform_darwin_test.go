//go:build darwin

package localrollback

import (
	"reflect"
	"testing"
)

type recordedCommandRunner struct {
	name string
	args []string
}

func (runner *recordedCommandRunner) Run(name string, args ...string) ([]byte, error) {
	runner.name = name
	runner.args = append([]string(nil), args...)
	return nil, nil
}

func TestDarwinSnapshotMountUsesSudoAndReadOnlyMountAPFS(t *testing.T) {
	runner := &recordedCommandRunner{}
	platform := &darwinVolumeSnapshotPlatform{runner: runner}
	err := platform.MountSnapshot(VolumeSnapshot{
		Name:       "com.apple.TimeMachine.2026-08-09-120000.local",
		MountPoint: "/System/Volumes/Data",
	}, "/private/tmp/unring-mount")
	if err != nil {
		t.Fatal(err)
	}
	if runner.name != "/usr/bin/sudo" {
		t.Fatalf("mount executable = %q, want /usr/bin/sudo", runner.name)
	}
	want := []string{
		"/sbin/mount_apfs", "-o", "rdonly", "-s",
		"com.apple.TimeMachine.2026-08-09-120000.local",
		"/System/Volumes/Data", "/private/tmp/unring-mount",
	}
	if !reflect.DeepEqual(runner.args, want) {
		t.Fatalf("mount arguments = %#v, want %#v", runner.args, want)
	}
}

func TestDarwinCreateParsesLiteralSnapshotName(t *testing.T) {
	runner := &literalOutputRunner{output: []byte("Created local snapshot with date: 2026-08-09-120000\n")}
	platform := &darwinVolumeSnapshotPlatform{runner: runner}
	name, err := platform.CreateSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if name != "com.apple.TimeMachine.2026-08-09-120000.local" {
		t.Fatalf("snapshot name = %q", name)
	}
}

type literalOutputRunner struct {
	output []byte
}

func (runner *literalOutputRunner) Run(string, ...string) ([]byte, error) {
	return runner.output, nil
}
