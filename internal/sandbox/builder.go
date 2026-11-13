package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	runtimeProjectionRoot = "/run/run-ai/provider-projections"
	wrapperSchema         = "run_ai_hard_readonly_wrapper/v1"
	selfTestMarker        = "run-ai-readonly-self-test/v1"
	maxRunnerOutput       = 16 << 10
	maxHashedFile         = 256 << 20
)

var sensitiveRoots = []string{
	"/root/.ssh",
	"/root/.codex",
	"/root/.claude",
	"/root/.gemini",
	"/run/credentials",
}

var allowedWorkspaceRoots = []string{
	"/var/www",
	"/srv",
}

// RuntimeBuilder returns the production detector. It performs no work until
// ValidateProjection or Build is called.
func RuntimeBuilder() Builder {
	return Builder{
		goos:            runtime.GOOS,
		wsl:             detectWSL(),
		projectionRoot:  runtimeProjectionRoot,
		inspect:         inspectPath,
		lookup:          exec.LookPath,
		resolve:         filepath.EvalSymlinks,
		digestFile:      secureFileDigest,
		run:             runCommand,
		selfTestTimeout: 8 * time.Second,
	}
}

// ValidateProjection proves that the profile refers to the fixed root-owned
// broker socket. It never opens or copies credential material.
func (b Builder) ValidateProjection(profile ProjectionProfile) (*ValidatedProjection, error) {
	if err := profile.Validate(); err != nil {
		return nil, b.block(BlockerProjectionInvalid, "projection profile validation failed")
	}
	if !b.projectionV2Verified {
		return nil, b.block(BlockerProjectionV2, "fixed broker validation of run, session, workflow plan, expiry, and generation is not implemented")
	}
	if !filepath.IsAbs(b.projectionRoot) || filepath.Clean(b.projectionRoot) != b.projectionRoot {
		return nil, b.block(BlockerProjectionInvalid, "projection root is not canonical")
	}
	root, err := b.inspect(b.projectionRoot)
	if err != nil || root.Kind != kindDirectory || root.UID != 0 || root.GID != 0 || root.Mode.Perm()&0o022 != 0 || root.Mode.Perm()&0o111 == 0 {
		return nil, b.block(BlockerProjectionInvalid, "projection root ownership or mode is unsafe")
	}
	hostSocket := filepath.Join(b.projectionRoot, profile.SocketName)
	socket, err := b.inspect(hostSocket)
	if err != nil || socket.Kind != kindSocket || socket.UID != profile.ExpectedUID || socket.GID != profile.ExpectedGID || uint32(socket.Mode.Perm()) != profile.ExpectedMode {
		return nil, b.block(BlockerProjectionInvalid, "projection socket identity does not match profile")
	}
	return &ValidatedProjection{
		profile: profile, root: b.projectionRoot, hostSocket: hostSocket,
		device: socket.Device, inode: socket.Inode,
	}, nil
}

// Build returns a wrapper only after the live projection, fixed provider
// adapter, bwrap executable, workspace and adversarial self-test all pass.
func (b Builder) Build(ctx context.Context, input BuildInput) (*Wrapper, error) {
	if b.goos == "darwin" {
		return nil, b.block(BlockerMacOSUnavailable, "Seatbelt adapter is intentionally not implemented")
	}
	if b.goos != "linux" {
		return nil, b.block(BlockerUnsupportedPlatform, "hard-readonly adapter is unavailable")
	}
	if b.wsl {
		return nil, b.block(BlockerWSLUnsupported, "WSL hard-readonly isolation has not been proven")
	}
	if input.Projection == nil {
		return nil, b.block(BlockerProjectionRequired, "validated provider credential projection is required")
	}
	projection := input.Projection
	if projection.profile.Provider != input.Provider || projection.root != b.projectionRoot {
		return nil, b.block(BlockerProjectionInvalid, "projection is bound to another provider or root")
	}
	if err := projection.profile.Validate(); err != nil {
		return nil, b.block(BlockerProjectionInvalid, "projection profile changed after validation")
	}
	socket, err := b.inspect(projection.hostSocket)
	if err != nil || socket.Kind != kindSocket || socket.Device != projection.device || socket.Inode != projection.inode ||
		socket.UID != projection.profile.ExpectedUID || socket.GID != projection.profile.ExpectedGID || uint32(socket.Mode.Perm()) != projection.profile.ExpectedMode {
		return nil, b.block(BlockerProjectionInvalid, "projection socket changed after validation")
	}

	workspace, err := b.validateWorkspace(input.Workspace)
	if err != nil {
		return nil, err
	}
	bwrapPath, err := b.lookup("bwrap")
	if err != nil {
		return nil, b.block(BlockerBwrapMissing, "bwrap executable was not found")
	}
	bwrapPath, err = b.trustedExecutable(bwrapPath)
	if err != nil || bwrapPath != "/usr/bin/bwrap" {
		return nil, b.block(BlockerBwrapUnsafe, "bwrap executable identity is not trusted")
	}

	adapterPath, err := b.trustedExecutable(projection.profile.ProviderAdapter)
	if err != nil || adapterPath != projection.profile.ProviderAdapter {
		return nil, b.block(BlockerProviderUnsupported, "fixed provider broker adapter is unavailable or unsafe")
	}
	adapterDigest, err := b.digestFile(adapterPath)
	if err != nil || adapterDigest != projection.profile.ProviderAdapterDigest {
		return nil, b.block(BlockerProviderUnsupported, "provider broker adapter digest mismatch")
	}
	// These probes are intentionally mandatory. A missing command inside an
	// `if command; then ...` shell test is indistinguishable from a correctly
	// denied connection, so validate their exact trusted executables first.
	for _, dependency := range []string{"/usr/bin/curl", "/usr/bin/ssh"} {
		resolved, err := b.trustedExecutable(dependency)
		if err != nil || resolved != dependency {
			return nil, b.block(BlockerRuntimeDependency, "mandatory readonly network probe is unavailable or unsafe")
		}
	}

	prefix := buildPrefix(workspace, projection.hostSocket)
	selfTestReceipt, err := b.selfTest(ctx, bwrapPath, prefix, projection.profile.ProfileDigest)
	if err != nil {
		return nil, err
	}
	descriptor := WrapperDescriptor{
		Schema: wrapperSchema, Provider: input.Provider, Executable: bwrapPath,
		PrefixArgs: append([]string(nil), prefix...), ProviderBinary: adapterPath,
		SandboxWorkingDirectory: "/workspace", ProjectionProfileDigest: projection.profile.ProfileDigest,
		SelfTestReceiptDigest: selfTestReceipt,
	}
	descriptor.WrapperDigest, err = digestWrapper(descriptor)
	if err != nil {
		return nil, b.block(BlockerSelfTestFailed, "wrapper receipt could not be created")
	}
	return &Wrapper{descriptor: descriptor, prefixArgs: append([]string(nil), prefix...)}, nil
}

func (w *Wrapper) Command(providerArgs []string) (Command, error) {
	if w == nil || w.descriptor.Schema != wrapperSchema || !validDigest(w.descriptor.WrapperDigest) {
		return Command{}, errors.New("invalid hard-readonly wrapper")
	}
	digest, err := digestWrapper(w.descriptor)
	if err != nil || digest != w.descriptor.WrapperDigest {
		return Command{}, errors.New("hard-readonly wrapper digest mismatch")
	}
	if len(providerArgs) > 1024 {
		return Command{}, errors.New("provider argument count exceeds bound")
	}
	total := 0
	for _, argument := range providerArgs {
		total += len(argument)
		if strings.IndexByte(argument, 0) >= 0 || len(argument) > 64<<10 || total > 1<<20 {
			return Command{}, errors.New("provider argument is unsafe or oversized")
		}
	}
	args := append([]string(nil), w.prefixArgs...)
	args = append(args, "--", w.descriptor.ProviderBinary)
	args = append(args, providerArgs...)
	return Command{Executable: w.descriptor.Executable, Args: args}, nil
}

func (b Builder) validateWorkspace(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || path == "/root" {
		return "", b.block(BlockerWorkspaceInvalid, "workspace path is not a narrow canonical absolute directory")
	}
	info, err := b.inspect(path)
	if err != nil || info.Kind != kindDirectory || info.Mode.Perm()&0o002 != 0 {
		return "", b.block(BlockerWorkspaceInvalid, "workspace is missing, a symlink, or not a directory")
	}
	allowed := false
	for _, root := range allowedWorkspaceRoots {
		if pathWithin(path, root) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", b.block(BlockerWorkspaceInvalid, "workspace is outside the fixed application roots")
	}
	for _, root := range append([]string{"/usr", "/etc", "/proc", "/sys", "/dev", "/run", "/bin", "/lib", "/lib64", "/sbin"}, sensitiveRoots...) {
		if pathWithin(path, root) || pathWithin(root, path) {
			return "", b.block(BlockerWorkspaceInvalid, "workspace overlaps a system or secret root")
		}
	}
	return path, nil
}

func (b Builder) trustedExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("noncanonical executable")
	}
	resolved, err := b.resolve(path)
	if err != nil || !filepath.IsAbs(resolved) || (resolved != "/usr/bin/bwrap" && !strings.HasPrefix(resolved, "/usr/")) {
		return "", errors.New("executable resolves outside /usr")
	}
	info, err := b.inspect(resolved)
	if err != nil || info.Kind != kindRegular || info.UID != 0 || info.GID != 0 || info.Mode.Perm()&0o022 != 0 || info.Mode.Perm()&0o111 == 0 {
		return "", errors.New("unsafe executable")
	}
	for current := filepath.Dir(resolved); current != "/"; current = filepath.Dir(current) {
		info, err := b.inspect(current)
		if err != nil || info.Kind != kindDirectory || info.UID != 0 || info.GID != 0 || info.Mode.Perm()&0o022 != 0 {
			return "", errors.New("unsafe executable ancestor")
		}
		if current == "/usr" {
			break
		}
	}
	return resolved, nil
}

func buildPrefix(workspace, hostSocket string) []string {
	return []string{
		"--die-with-parent", "--new-session", "--unshare-all", "--unshare-user", "--disable-userns", "--cap-drop", "ALL",
		"--clearenv", "--hostname", "run-ai-readonly",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
		"--proc", "/proc", "--dev", "/dev", "--dir", "/etc",
		"--tmpfs", "/tmp", "--dir", "/tmp/home", "--dir", "/tmp/config", "--dir", "/tmp/cache",
		"--dir", "/run", "--dir", "/run/run-ai", "--ro-bind", hostSocket, sandboxBrokerSocket,
		"--dir", "/workspace", "--ro-bind", workspace, "/workspace", "--chdir", "/workspace",
		"--setenv", "HOME", "/tmp/home", "--setenv", "XDG_CONFIG_HOME", "/tmp/config",
		"--setenv", "XDG_CACHE_HOME", "/tmp/cache", "--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "LANG", "C.UTF-8", "--setenv", "RUN_AI_HARD_READONLY", "1",
		"--setenv", "RUN_AI_PROVIDER_BROKER_SOCKET", sandboxBrokerSocket,
	}
}

func (b Builder) selfTest(parent context.Context, executable string, prefix []string, projectionDigest string) (string, error) {
	timeout := b.selfTestTimeout
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	args := append([]string(nil), prefix...)
	args = append(args, "--", "/bin/sh", "-ceu", selfTestScript)
	result, err := b.run(ctx, executable, args)
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Output) != selfTestMarker {
		return "", b.block(BlockerSelfTestFailed, "bwrap adversarial self-test did not prove the boundary")
	}
	receipt := struct {
		Schema           string `json:"schema"`
		ProjectionDigest string `json:"projection_digest"`
		PrefixDigest     string `json:"prefix_digest"`
		Marker           string `json:"marker"`
	}{
		Schema: "run_ai_readonly_self_test_receipt/v1", ProjectionDigest: projectionDigest,
		PrefixDigest: digestStrings(prefix), Marker: selfTestMarker,
	}
	payload, _ := json.Marshal(receipt)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

const selfTestScript = `
test -S /run/run-ai/provider-api.sock
test -w /tmp
if ( : > /workspace/.run-ai-readonly-write-probe ) 2>/dev/null; then exit 70; fi
if ( : > /usr/.run-ai-readonly-write-probe ) 2>/dev/null; then exit 71; fi
for secret in /root/.ssh /root/.codex /root/.claude /root/.gemini /run/credentials; do
  if test -e "$secret"; then exit 72; fi
done
if /usr/bin/grep -Eq '^[^[:space:]]+[[:space:]]+00000000([[:space:]]|$)' /proc/net/route; then exit 73; fi
if /usr/bin/curl --noproxy '*' --connect-timeout 1 --max-time 2 -fsS http://192.0.2.1/ >/dev/null 2>&1; then exit 74; fi
if /usr/bin/ssh -F /dev/null -oBatchMode=yes -oConnectTimeout=1 -oConnectionAttempts=1 192.0.2.1 true >/dev/null 2>&1; then exit 75; fi
printf '%s\n' 'run-ai-readonly-self-test/v1'
`

func (b Builder) block(code BlockerCode, detail string) error {
	platform := b.goos
	if b.wsl {
		platform = "wsl"
	}
	return &Blocker{Code: code, Platform: platform, Detail: detail}
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func digestStrings(values []string) string {
	payload, _ := json.Marshal(values)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func digestWrapper(descriptor WrapperDescriptor) (string, error) {
	descriptor.WrapperDigest = ""
	payload, err := json.Marshal(descriptor)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func detectWSL() bool {
	if os.Getenv("WSL_INTEROP") != "" || os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	for _, path := range []string{"/proc/sys/kernel/osrelease", "/proc/version"} {
		payload, err := os.ReadFile(path)
		if err == nil && strings.Contains(strings.ToLower(string(payload)), "microsoft") {
			return true
		}
	}
	return false
}

func inspectPath(path string) (pathInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return pathInfo{}, err
	}
	result := pathInfo{Mode: info.Mode()}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		result.Kind = kindSymlink
	case info.Mode().IsRegular():
		result.Kind = kindRegular
	case info.IsDir():
		result.Kind = kindDirectory
	case info.Mode()&os.ModeSocket != 0:
		result.Kind = kindSocket
	default:
		result.Kind = kindOther
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return pathInfo{}, errors.New("path stat identity is unavailable")
	}
	result.UID, result.GID = stat.Uid, stat.Gid
	result.Device, result.Inode = uint64(stat.Dev), stat.Ino
	return result, nil
}

func secureFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > maxHashedFile {
		return "", errors.New("unsafe digest source")
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(file, maxHashedFile+1))
	if err != nil || written != before.Size() {
		return "", errors.New("digest source changed or exceeded bound")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return "", errors.New("digest source identity changed")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedBuffer) Write(payload []byte) (int, error) {
	original := len(payload)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(payload) > remaining {
			payload = payload[:remaining]
		}
		_, _ = b.buffer.Write(payload)
	}
	return original, nil
}

func runCommand(ctx context.Context, executable string, args []string) (runResult, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	var output boundedBuffer
	output.limit = maxRunnerOutput
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	result := runResult{Output: output.buffer.String()}
	if err == nil {
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, err
	}
	return result, err
}
