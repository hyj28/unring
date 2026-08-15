//go:build darwin

package localrollback

import (
	"context"
	"reflect"
	"strings"
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

func (runner *recordedCommandRunner) RunContext(_ context.Context, name string, args ...string) ([]byte, error) {
	return runner.Run(name, args...)
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

func TestDarwinExcludedBatchMapsIndependentLiteralStatusesInOrder(t *testing.T) {
	runner := &literalOutputRunner{output: []byte(
		"[Included] /literal/kept.txt\n[Excluded] /literal/skipped.txt\n",
	)}
	platform := &darwinVolumeSnapshotPlatform{runner: runner}
	statuses, err := platform.IsExcludedBatch(context.Background(), []string{
		"/literal/kept.txt", "/literal/skipped.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{false, true}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("excluded statuses = %#v, want independent literal %#v", statuses, want)
	}
}

func TestDarwinExcludedBatchMatchesNFDOutputToNFCRequests(t *testing.T) {
	runner := &literalOutputRunner{output: []byte(
		"[Included] /literal/cafe\u0301.txt\n" +
			"[Excluded] /literal/Mu\u0308ller.txt\n" +
			"[Included] /literal/\u1112\u1161\u11ab\u1100\u1173\u11af.txt\n",
	)}
	platform := &darwinVolumeSnapshotPlatform{runner: runner}
	statuses, err := platform.IsExcludedBatch(context.Background(), []string{
		"/literal/café.txt", "/literal/Müller.txt", "/literal/한글.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{false, true, false}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("normalized statuses = %#v, want independent literal %#v", statuses, want)
	}
}

func TestDarwinSingleExcludedCheckPreservesWhitespaceAndLineBreaks(t *testing.T) {
	paths := []string{
		"/literal/draft .txt",
		"/literal/ leading.txt",
		"/literal/line\nbreak.txt",
	}
	for _, path := range paths {
		t.Run(strings.ReplaceAll(path, "\n", "-newline-"), func(t *testing.T) {
			platform := &darwinVolumeSnapshotPlatform{
				runner: &literalOutputRunner{output: []byte("[Excluded] " + path + "\n")},
			}
			statuses, err := platform.IsExcludedBatch(context.Background(), []string{path})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(statuses, []bool{true}) {
				t.Fatalf("single-path status = %#v, want independent literal [true]", statuses)
			}
		})
	}
}

func TestDarwinExcludedBatchRejectsUnattributableOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "short", output: "[Included] /literal/first\n"},
		{name: "reordered", output: "[Excluded] /literal/second\n[Included] /literal/first\n"},
		{name: "malformed", output: "[Included] /literal/first\n[Unknown] /literal/second\n"},
		{name: "diagnostic", output: "tmutil: diagnostic\n[Included] /literal/first\n[Excluded] /literal/second\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := &darwinVolumeSnapshotPlatform{
				runner: &literalOutputRunner{output: []byte(test.output)},
			}
			_, err := platform.IsExcludedBatch(context.Background(), []string{
				"/literal/first", "/literal/second",
			})
			if err == nil || !strings.Contains(err.Error(), "unexpected tmutil isexcluded output") {
				t.Fatalf("error = %v, want explicit unattributable-output failure", err)
			}
		})
	}
}

type literalOutputRunner struct {
	output []byte
}

func (runner *literalOutputRunner) Run(string, ...string) ([]byte, error) {
	return runner.output, nil
}

func (runner *literalOutputRunner) RunContext(context.Context, string, ...string) ([]byte, error) {
	return runner.output, nil
}
