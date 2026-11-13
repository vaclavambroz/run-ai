package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPlatformAndProjectionFailClosed(t *testing.T) {
	tests := []struct {
		name string
		goos string
		wsl  bool
		code BlockerCode
	}{
		{name: "WSL", goos: "linux", wsl: true, code: BlockerWSLUnsupported},
		{name: "macOS placeholder", goos: "darwin", code: BlockerMacOSUnavailable},
		{name: "unsupported", goos: "windows", code: BlockerUnsupportedPlatform},
		{name: "Linux projection required", goos: "linux", code: BlockerProjectionRequired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder := Builder{goos: test.goos, wsl: test.wsl}
			_, err := builder.Build(context.Background(), BuildInput{Provider: "codex", Workspace: "/workspace"})
			assertBlocker(t, err, test.code)
		})
	}
}

func TestProjectionProfileRejectsCredentialVisibilityAndTampering(t *testing.T) {
	profile, err := NewProjectionProfile("codex", 1001, 1002, 0o660, testDigest, testDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	profile.CredentialMaterialVisible = true
	profile.ProfileDigest, _ = digestProjectionProfile(profile)
	if err := profile.Validate(); err == nil {
		t.Fatal("credential-file projection accepted")
	}

	profile, _ = NewProjectionProfile("codex", 1001, 1002, 0o660, testDigest, testDigest)
	profile.SessionBindingRequired = false
	profile.ArbitraryDestinationAllowed = true
	profile.ProfileDigest, _ = digestProjectionProfile(profile)
	if err := profile.Validate(); err == nil {
		t.Fatal("unbound arbitrary-destination broker accepted")
	}

	profile, _ = NewProjectionProfile("codex", 1001, 1002, 0o660, testDigest, testDigest)
	profile.SocketName = "../../root/.ssh/id_ed25519"
	profile.ProfileDigest, _ = digestProjectionProfile(profile)
	if err := profile.Validate(); err == nil {
		t.Fatal("projection path escape accepted")
	}

	if _, err := NewProjectionProfile("unknown", 0, 0, 0o600, testDigest, testDigest); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestValidateProjectionChecksRootAndSocketIdentity(t *testing.T) {
	builder, profile, paths := fakeBuilder(t, "codex")
	projection, err := builder.ValidateProjection(profile)
	if err != nil {
		t.Fatal(err)
	}
	if projection.hostSocket != "/run/test-projections/codex.sock" {
		t.Fatalf("socket = %q", projection.hostSocket)
	}

	unsafe := paths[builder.projectionRoot]
	unsafe.Mode = 0o777 | fs.ModeDir
	paths[builder.projectionRoot] = unsafe
	_, err = builder.ValidateProjection(profile)
	assertBlocker(t, err, BlockerProjectionInvalid)

	builder, profile, paths = fakeBuilder(t, "codex")
	socket := paths["/run/test-projections/codex.sock"]
	socket.GID++
	paths["/run/test-projections/codex.sock"] = socket
	_, err = builder.ValidateProjection(profile)
	assertBlocker(t, err, BlockerProjectionInvalid)
}

func TestRuntimeProjectionV1CannotClaimSessionBinding(t *testing.T) {
	profile, err := NewProjectionProfile("codex", 1001, 1002, 0o660, testDigest, testDigest)
	if err != nil {
		t.Fatal(err)
	}
	builder := RuntimeBuilder()
	_, err = builder.ValidateProjection(profile)
	assertBlocker(t, err, BlockerProjectionV2)
}

func TestBuildCreatesFixedWrapperAfterAdversarialSelfTest(t *testing.T) {
	builder, profile, _ := fakeBuilder(t, "codex")
	projection, err := builder.ValidateProjection(profile)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	builder.run = func(_ context.Context, executable string, args []string) (runResult, error) {
		called = true
		if executable != "/usr/bin/bwrap" {
			t.Fatalf("executable = %q", executable)
		}
		joined := strings.Join(args, "\n")
		for _, required := range []string{
			"--unshare-all", "--unshare-user", "--disable-userns", "--clearenv", "--cap-drop",
			"/workspace/.run-ai-readonly-write-probe", "/usr/bin/curl", "/usr/bin/ssh",
			"/root/.ssh", "/root/.codex", "/run/credentials",
			"/run/run-ai/provider-api.sock",
		} {
			if !strings.Contains(joined, required) {
				t.Fatalf("self-test args lack %q", required)
			}
		}
		if slices.Contains(args, "--share-net") {
			t.Fatal("sandbox retained host network")
		}
		return runResult{Output: selfTestMarker + "\n"}, nil
	}
	wrapper, err := builder.Build(context.Background(), BuildInput{
		Provider: "codex", Workspace: "/srv/app", Projection: projection,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("runtime self-test was not executed")
	}
	descriptor := wrapper.Descriptor()
	if descriptor.ProviderBinary != "/usr/local/libexec/run-ai/codex-readonly" || !validDigest(descriptor.WrapperDigest) || !validDigest(descriptor.SelfTestReceiptDigest) {
		t.Fatalf("invalid wrapper descriptor: %+v", descriptor)
	}
	command, err := wrapper.Command([]string{"--model", "gpt-5", "untrusted text; $(touch /tmp/x)"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Executable != "/usr/bin/bwrap" || !slices.Contains(command.Args, descriptor.ProviderBinary) {
		t.Fatalf("wrapper command escaped fixed executable: %+v", command)
	}
	if _, err := wrapper.Command([]string{"bad\x00argument"}); err == nil {
		t.Fatal("NUL provider argument accepted")
	}

	copyDescriptor := wrapper.Descriptor()
	copyDescriptor.PrefixArgs[0] = "--share-net"
	if wrapper.Descriptor().PrefixArgs[0] == "--share-net" {
		t.Fatal("descriptor exposed mutable wrapper argv")
	}
}

func TestBuildRechecksProjectionAndAdapter(t *testing.T) {
	builder, profile, paths := fakeBuilder(t, "claude")
	projection, err := builder.ValidateProjection(profile)
	if err != nil {
		t.Fatal(err)
	}
	socket := paths[projection.hostSocket]
	socket.Inode++
	paths[projection.hostSocket] = socket
	_, err = builder.Build(context.Background(), BuildInput{Provider: "claude", Workspace: "/srv/app", Projection: projection})
	assertBlocker(t, err, BlockerProjectionInvalid)

	builder, profile, _ = fakeBuilder(t, "claude")
	projection, _ = builder.ValidateProjection(profile)
	builder.digestFile = func(string) (string, error) { return strings.Repeat("f", 64), nil }
	_, err = builder.Build(context.Background(), BuildInput{Provider: "claude", Workspace: "/srv/app", Projection: projection})
	assertBlocker(t, err, BlockerProviderUnsupported)

	builder, profile, paths = fakeBuilder(t, "claude")
	projection, _ = builder.ValidateProjection(profile)
	delete(paths, "/usr/bin/curl")
	_, err = builder.Build(context.Background(), BuildInput{Provider: "claude", Workspace: "/srv/app", Projection: projection})
	assertBlocker(t, err, BlockerRuntimeDependency)
}

func TestBuildRejectsSensitiveWorkspaceAndFailedSelfTest(t *testing.T) {
	builder, profile, paths := fakeBuilder(t, "antigravity")
	paths["/root/.ssh"] = pathInfo{Kind: kindDirectory, Mode: fs.ModeDir | 0o700, UID: 0, GID: 0}
	projection, _ := builder.ValidateProjection(profile)
	_, err := builder.Build(context.Background(), BuildInput{Provider: "antigravity", Workspace: "/root/.ssh", Projection: projection})
	assertBlocker(t, err, BlockerWorkspaceInvalid)

	builder, profile, _ = fakeBuilder(t, "antigravity")
	projection, _ = builder.ValidateProjection(profile)
	builder.run = func(context.Context, string, []string) (runResult, error) {
		return runResult{ExitCode: 74, Output: "network unexpectedly available"}, errors.New("exit 74")
	}
	_, err = builder.Build(context.Background(), BuildInput{Provider: "antigravity", Workspace: "/srv/app", Projection: projection})
	assertBlocker(t, err, BlockerSelfTestFailed)
}

func TestLiveBwrapSelfTestWhenAvailable(t *testing.T) {
	if runtimeBuilder := RuntimeBuilder(); runtimeBuilder.goos != "linux" || runtimeBuilder.wsl {
		t.Skip("Linux non-WSL integration only")
	}
	bwrap, err := execLookPath("bwrap")
	if err != nil || bwrap != "/usr/bin/bwrap" {
		t.Skip("fixed bwrap is unavailable")
	}
	workspace := t.TempDir()
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "broker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("cannot create Unix test socket: %v", err)
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	prefix := buildPrefix(workspace, socketPath)
	args := append(append([]string(nil), prefix...), "--", "/bin/sh", "-ceu", selfTestScript)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runCommand(ctx, bwrap, args)
	if err != nil {
		t.Skipf("host kernel did not permit bwrap integration probe: exit=%d output=%q err=%v", result.ExitCode, result.Output, err)
	}
	if strings.TrimSpace(result.Output) != selfTestMarker {
		t.Fatalf("unexpected self-test output %q", result.Output)
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".run-ai-readonly-write-probe")); !os.IsNotExist(err) {
		t.Fatal("readonly self-test wrote into host workspace")
	}
}

func fakeBuilder(t *testing.T, provider string) (Builder, ProjectionProfile, map[string]pathInfo) {
	t.Helper()
	profile, err := NewProjectionProfile(provider, 1001, 1002, 0o660, testDigest, testDigest)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]pathInfo{
		"/run/test-projections":                       {Kind: kindDirectory, Mode: fs.ModeDir | 0o750, UID: 0, GID: 0, Device: 1, Inode: 1},
		"/run/test-projections/" + profile.SocketName: {Kind: kindSocket, Mode: fs.ModeSocket | 0o660, UID: 1001, GID: 1002, Device: 1, Inode: 2},
		"/srv/app":                            {Kind: kindDirectory, Mode: fs.ModeDir | 0o755, UID: 2000, GID: 2000, Device: 2, Inode: 3},
		"/usr/bin/bwrap":                      {Kind: kindRegular, Mode: 0o755, UID: 0, GID: 0},
		"/usr/bin/curl":                       {Kind: kindRegular, Mode: 0o755, UID: 0, GID: 0},
		"/usr/bin/ssh":                        {Kind: kindRegular, Mode: 0o755, UID: 0, GID: 0},
		"/usr/bin":                            {Kind: kindDirectory, Mode: fs.ModeDir | 0o755, UID: 0, GID: 0},
		"/usr":                                {Kind: kindDirectory, Mode: fs.ModeDir | 0o755, UID: 0, GID: 0},
		profile.ProviderAdapter:               {Kind: kindRegular, Mode: 0o755, UID: 0, GID: 0},
		filepath.Dir(profile.ProviderAdapter): {Kind: kindDirectory, Mode: fs.ModeDir | 0o755, UID: 0, GID: 0},
		filepath.Dir(filepath.Dir(profile.ProviderAdapter)): {Kind: kindDirectory, Mode: fs.ModeDir | 0o755, UID: 0, GID: 0},
		"/usr/local": {Kind: kindDirectory, Mode: fs.ModeDir | 0o755, UID: 0, GID: 0},
	}
	builder := Builder{
		goos: "linux", projectionRoot: "/run/test-projections", selfTestTimeout: time.Second,
		projectionV2Verified: true,
		lookup: func(name string) (string, error) {
			if name != "bwrap" {
				return "", errors.New("not found")
			}
			return "/usr/bin/bwrap", nil
		},
		resolve: func(path string) (string, error) { return path, nil },
		inspect: func(path string) (pathInfo, error) {
			info, ok := paths[path]
			if !ok {
				return pathInfo{}, os.ErrNotExist
			}
			return info, nil
		},
		digestFile: func(path string) (string, error) {
			if path != profile.ProviderAdapter {
				return "", errors.New("unexpected digest path")
			}
			return testDigest, nil
		},
		run: func(context.Context, string, []string) (runResult, error) {
			return runResult{Output: selfTestMarker + "\n"}, nil
		},
	}
	return builder, profile, paths
}

func assertBlocker(t *testing.T, err error, code BlockerCode) {
	t.Helper()
	var blocker *Blocker
	if !errors.As(err, &blocker) || blocker.Code != code {
		t.Fatalf("error = %v, want blocker %s", err, code)
	}
}

var execLookPath = func(name string) (string, error) {
	return RuntimeBuilder().lookup(name)
}
