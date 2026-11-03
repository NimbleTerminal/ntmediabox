// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: AGPL-3.0-only WITH LicenseRef-NimbleTerminal-Linking-Exception OR LicenseRef-Neomantra-Commercial

package ntmediabox

import (
	"io"
	"os/exec"

	"nimbleterminal.dev/ntconfine"
)

const (
	// StageArg is the CLI flag passed to ntconfine-stage before the spec token or "fd:<N>".
	StageArg = ntconfine.StageArg

	// SpecVersion is the wire version of StageSpec.
	SpecVersion = ntconfine.SpecVersion

	// DefaultStageBinary is the default standalone stage executable name ("ntconfine-stage").
	DefaultStageBinary = ntconfine.DefaultStageBinary
)

// ErrUnavailable means this machine cannot enforce the requested OS sandbox policy.
var ErrUnavailable = ntconfine.ErrUnavailable

// Mechanism names the OS sandbox mechanism used to confine a worker process.
type Mechanism = ntconfine.Mechanism

const (
	MechanismNone        = ntconfine.MechanismNone
	MechanismLandlock    = ntconfine.MechanismLandlock
	MechanismSandboxExec = ntconfine.MechanismSandboxExec
)

// Support describes the OS sandbox enforcement capability of the current host
// or of a running Helper.
type Support = ntconfine.Support

// Policy defines the OS confinement for a media or render worker process.
type Policy = ntconfine.Policy

// StageSpec is the language-neutral launch specification passed to ntconfine-stage.
type StageSpec = ntconfine.StageSpec

// EncodeStageSpec serializes spec as JSON and encodes it as raw URL-safe base64.
var EncodeStageSpec = ntconfine.EncodeStageSpec

// DecodeStageSpec decodes a raw URL-safe base64 StageSpec token.
var DecodeStageSpec = ntconfine.DecodeStageSpec

// HelperConfig configures how StartHelper or Command launches a confined worker.
type HelperConfig = ntconfine.HelperConfig

// Helper is a running worker process supervised by the host.
type Helper = ntconfine.Helper

// DefaultPolicy returns the standard worker policy for helperBin reading from
// readPaths (such as a private staging directory and any resolved media file).
// WritePaths is nil and AllowUnsandboxed is false.
func DefaultPolicy(helperBin string, readPaths ...string) Policy {
	return ntconfine.DefaultPolicy(helperBin, readPaths...)
}

// DefaultLibPaths returns the default shared-library and font search directories
// for a confined worker on the current platform.
func DefaultLibPaths() []string {
	return ntconfine.DefaultLibPaths()
}

// DefaultExecPaths returns the default dynamic loader paths required for execve
// on the current platform.
func DefaultExecPaths() []string {
	return ntconfine.DefaultExecPaths()
}

// Available reports whether OS sandbox enforcement is available on the current host.
func Available() bool {
	return ntconfine.Available()
}

// Probe reports the OS sandbox enforcement capability of the current host.
func Probe() Support {
	return ntconfine.Probe()
}

// Command constructs the confined (or unsandboxed fallback when
// Policy.AllowUnsandboxed is true and OS enforcement is unavailable) *exec.Cmd
// for a worker without starting it or attaching a socketpair.
func Command(cfg HelperConfig) (*exec.Cmd, Support, error) {
	return ntconfine.Command(cfg)
}

// StartHelper starts a confined worker process before the host locks itself down.
func StartHelper(cfg HelperConfig) (*Helper, error) {
	return ntconfine.StartHelper(cfg)
}

// RunStage parses the encoded stage specification from args and replaces the
// current process with the confined worker.
func RunStage(args []string) error {
	return ntconfine.RunStage(args)
}

// RunStagePolicy confines the current process according to pol and replaces it
// with workerPath.
func RunStagePolicy(pol Policy, workerPath string, workerArgs []string) error {
	return ntconfine.RunStagePolicy(pol, workerPath, workerArgs)
}

// WorkerConn returns the worker side's connection to the host (fd 3 on Unix,
// stdin/stdout on Windows).
func WorkerConn() io.ReadWriteCloser {
	return ntconfine.WorkerConn()
}

// KillHelper terminates a worker process (and its process group on Unix) by PID.
func KillHelper(pid int) {
	ntconfine.KillHelper(pid)
}

// ResolveSibling resolves a binary by bare name in the same directory as the
// current executable and verifies its ownership and permissions with VerifyBinary.
func ResolveSibling(name string) (string, error) {
	return ntconfine.ResolveSibling(name)
}

// VerifyBinary resolves symlinks on path and checks that it is a regular
// executable file with safe ownership and permissions.
func VerifyBinary(path string) (string, error) {
	return ntconfine.VerifyBinary(path)
}
