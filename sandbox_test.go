// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: AGPL-3.0-only WITH LicenseRef-NimbleTerminal-Linking-Exception OR LicenseRef-Neomantra-Commercial

package ntmediabox

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	probeMarker   = "nt-helper-probe"
	hostileMarker = "nt-helper-hostile"
	sleepMarker   = "nt-helper-sleep"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == StageArg {
		fmt.Println(RunStage(os.Args[1:])) // helper stage: never returns on success
		os.Exit(9)
	}
	if len(os.Args) >= 2 && os.Args[1] == hostileMarker {
		fmt.Fprint(os.Stderr, "worker-stderr\x1b[31m-line\n")
		conn := WorkerConn()
		_, _ = conn.Write([]byte{0xff, 0xff, 0xff, 0xff, 99})
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	if len(os.Args) >= 2 && os.Args[1] == sleepMarker {
		_ = WorkerConn()
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	if len(os.Args) >= 2 && strings.Contains(os.Args[1], probeMarker) {
		var singleFile, siblingFile string
		if len(os.Args) >= 4 {
			singleFile = os.Args[2]
			siblingFile = os.Args[3]
		}
		helperProbe(os.Args[1], singleFile, siblingFile)
	}
	os.Exit(m.Run())
}

func helperProbe(tmp, singleFile, siblingFile string) {
	conn := WorkerConn()
	report := func(name string, err error) { _, _ = fmt.Fprintf(conn, "%s=%v\n", name, err == nil) }
	_, _ = fmt.Fprintf(conn, "env=%d\n", len(os.Environ()))
	c, err := net.DialTimeout("tcp", "1.1.1.1:443", 500*time.Millisecond)
	if c != nil {
		_ = c.Close()
	}
	report("network", err)
	_, err = os.ReadFile("/etc/passwd")
	report("passwd", err)
	_, err = os.ReadFile(filepath.Join(tmp, "clip"))
	report("readtemp", err)
	report("writetemp", os.WriteFile(filepath.Join(tmp, "new"), []byte("x"), 0o600))
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if f != nil {
		_ = f.Close()
	}
	report("tty", err)
	victim := tmp + "-victim"
	report("truncate", os.Truncate(victim, 0))
	report("chmod", os.Chmod(victim, 0o777))
	if singleFile != "" {
		_, err = os.ReadFile(singleFile)
		report("singlefile", err)
		tf, terr := os.OpenFile(singleFile, os.O_RDONLY|os.O_TRUNC, 0)
		if tf != nil {
			_ = tf.Close()
		}
		report("opentrunc", terr)
		_, err = os.ReadFile(filepath.Join(tmp, "linked-clip"))
		report("linkedfile", err)
	}
	if siblingFile != "" {
		_, err = os.ReadFile(siblingFile)
		report("siblingfile", err)
		_, err = os.ReadDir(filepath.Dir(siblingFile))
		report("siblingdir", err)
	}
	_, _ = fmt.Fprintln(conn, "done")
	_ = conn.Close()
	os.Exit(0)
}

func runProbe(t *testing.T) map[string]string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp("", probeMarker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	if tmp, err = filepath.EvalSymlinks(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "clip"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp+"-victim", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(tmp + "-victim") })

	outsideDir, err := os.MkdirTemp("", "nt-helper-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outsideDir) })
	if outsideDir, err = filepath.EvalSymlinks(outsideDir); err != nil {
		t.Fatal(err)
	}
	singleFile := filepath.Join(outsideDir, "allowed.mp4")
	siblingFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(singleFile, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(siblingFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(singleFile, filepath.Join(tmp, "linked-clip")); err != nil {
		t.Fatal(err)
	}

	pol := DefaultPolicy(exe, tmp, singleFile)
	h, err := StartHelper(HelperConfig{
		Policy:     pol,
		WorkerPath: exe,
		WorkerArgs: []string{tmp, singleFile, siblingFile},
		StagePath:  exe,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if !h.Enforced() {
		t.Fatal("expected helper to report Enforced() == true")
	}
	if f, ok := h.Conn.(*os.File); ok {
		_ = f.SetDeadline(time.Now().Add(10 * time.Second))
	}
	got := map[string]string{}
	r := bufio.NewReader(h.Conn)
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF || err != nil || strings.TrimSpace(line) == "done" {
			break
		}
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		got[k] = v
	}
	return got
}

func TestHelperIsConfined(t *testing.T) {
	if !Available() {
		if os.Getenv("NTMEDIABOX_REQUIRE_SANDBOX") == "1" {
			t.Fatal("OS enforcement unavailable here (NTMEDIABOX_REQUIRE_SANDBOX=1)")
		}
		t.Skip("OS enforcement unavailable here")
	}
	got := runProbe(t)
	want := map[string]string{
		"env":         "0",
		"network":     "false",
		"passwd":      "false",
		"readtemp":    "true",
		"writetemp":   "false",
		"tty":         "false",
		"truncate":    "false",
		"chmod":       "false",
		"singlefile":  "true",
		"opentrunc":   "false",
		"linkedfile":  "true",
		"siblingfile": "false",
		"siblingdir":  "false",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

func TestZeroPolicyIsClosed(t *testing.T) {
	var zero Policy
	if zero.AllowUnsandboxed {
		t.Fatal("zero policy must not allow unsandboxed execution")
	}
	if err := zero.Validate(); err == nil {
		t.Fatal("zero policy must fail Validate")
	}
	pol := DefaultPolicy("/bin/echo", "/tmp")
	if pol.AllowUnsandboxed {
		t.Fatal("DefaultPolicy must not set AllowUnsandboxed")
	}
	if len(pol.WritePaths) != 0 {
		t.Fatalf("DefaultPolicy must have empty WritePaths, got %v", pol.WritePaths)
	}
	if err := pol.Validate(); err != nil {
		t.Fatalf("DefaultPolicy failed Validate: %v", err)
	}
}

func TestProbeReportsPlatformSupport(t *testing.T) {
	sup := Probe()
	switch runtime.GOOS {
	case "darwin":
		if !sup.Deprecated {
			t.Fatal("macOS sandbox-exec must be marked Deprecated")
		}
		if sup.Available && sup.Mechanism != MechanismSandboxExec {
			t.Fatalf("macOS mechanism = %q, want %q", sup.Mechanism, MechanismSandboxExec)
		}
	case "linux":
		if sup.Available {
			if sup.Mechanism != MechanismLandlock || sup.LandlockABI < 1 {
				t.Fatalf("linux support = %+v", sup)
			}
		}
	default:
		if sup.Available || sup.Mechanism != MechanismNone {
			t.Fatalf("unexpected support on %s: %+v", runtime.GOOS, sup)
		}
	}
}

func TestStartMediaHelperKillsOnProtocolViolationAndCapturesStderr(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	pol := DefaultPolicy(exe)
	pol.AllowUnsandboxed = true

	client, helper, err := StartMediaHelper(HelperConfig{
		Policy:     pol,
		WorkerPath: exe,
		WorkerArgs: []string{hostileMarker},
		StagePath:  exe,
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Close() }()

	select {
	case <-client.Dead():
	case <-time.After(5 * time.Second):
		t.Fatal("StartMediaHelper did not kill and reap hostile worker on protocol violation")
	}
	if !errors.Is(client.Err(), ErrProtocol) {
		t.Fatalf("client.Err() = %v, want ErrProtocol", client.Err())
	}
	if got := helper.Stderr(); !strings.Contains(got, "worker-stderr") || strings.Contains(got, "\x1b") {
		t.Fatalf("helper.Stderr() = %q, want control-stripped worker-stderr", got)
	}
}

func TestHelperTransportSupportsDeadlineAndCloseUnblocksRead(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unix socketpair transport test")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	pol := DefaultPolicy(exe)
	pol.AllowUnsandboxed = true

	helper, err := StartHelper(HelperConfig{
		Policy:     pol,
		WorkerPath: exe,
		WorkerArgs: []string{sleepMarker},
		StagePath:  exe,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Close() }()

	f, ok := helper.Conn.(*os.File)
	if !ok {
		t.Fatalf("helper.Conn = %T, want *os.File", helper.Conn)
	}
	if err := f.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetWriteDeadline on socketpair failed: %v", err)
	}
	if err := f.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("clearing SetWriteDeadline failed: %v", err)
	}

	readDone := make(chan error, 1)
	go func() {
		var buf [1]byte
		_, err := helper.Conn.Read(buf[:])
		readDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := helper.Conn.Close(); err != nil {
		t.Fatalf("helper.Conn.Close: %v", err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("expected Read on closed helper.Conn to fail")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("helper.Conn.Close() did not unblock concurrent Read()")
	}
}

func TestCommandDoesNotPutPolicyPathsOnCommandLine(t *testing.T) {
	if !Available() {
		t.Skip("OS enforcement unavailable here")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	secretPath := "/tmp/nonexistent-secret-media-file-998877.mp4"
	pol := DefaultPolicy(exe, secretPath)
	cmd, sup, err := Command(HelperConfig{
		Policy:     pol,
		WorkerPath: exe,
		StagePath:  exe,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, f := range cmd.ExtraFiles {
			if f != nil {
				_ = f.Close()
			}
		}
	}()
	if !sup.Available {
		t.Fatal("expected Command Support.Available == true")
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "nonexistent-secret-media-file") {
			t.Fatalf("Command exposed policy path in argv: %q", arg)
		}
		if len(arg) > 256 {
			t.Fatalf("Command passed large inline token (%d bytes) in argv", len(arg))
		}
	}
}

func FuzzPolicyValidate(f *testing.F) {
	f.Add("/bin/echo", "/tmp", "/var/tmp")
	f.Add("relative/path", "/tmp/bad\npath", "/tmp/bad\x00path")
	f.Add("/usr/bin/true", "/opt/homebrew/lib", "/Library/Fonts")

	f.Fuzz(func(t *testing.T, execPath, readPath, writePath string) {
		pol := Policy{
			ExecPaths:  []string{execPath},
			ReadPaths:  []string{readPath},
			WritePaths: []string{writePath},
		}
		if err := pol.Validate(); err == nil {
			for _, p := range []string{execPath, readPath, writePath} {
				if !filepath.IsAbs(p) {
					t.Fatalf("Policy.Validate accepted non-absolute path %q", p)
				}
				for _, r := range p {
					if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
						t.Fatalf("Policy.Validate accepted control rune %U in %q", r, p)
					}
				}
			}
		}
	})
}
