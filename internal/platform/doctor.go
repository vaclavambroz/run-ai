package platform

import (
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

type BinaryCheck struct {
	Name  string `json:"name"`
	Path  string `json:"path,omitempty"`
	Found bool   `json:"found"`
}

type Report struct {
	OS                        string        `json:"os"`
	Arch                      string        `json:"arch"`
	WSL                       bool          `json:"wsl"`
	Systemd                   bool          `json:"systemd"`
	UserNamespaces            bool          `json:"user_namespaces"`
	SandboxHelper             string        `json:"sandbox_helper,omitempty"`
	SandboxPrimitiveReady     bool          `json:"sandbox_primitive_ready"`
	CredentialProjectionReady bool          `json:"credential_projection_ready"`
	Binaries                  []BinaryCheck `json:"binaries"`
	ReadonlyEnforce           bool          `json:"readonly_enforceable"`
	Warnings                  []string      `json:"warnings,omitempty"`
}

// Doctor is intentionally read-only. It describes platform prerequisites but
// never installs a package, rewrites provider config, or changes authentication.
func Doctor(providerBinary string, lookup func(string) (string, error), readFile func(string) ([]byte, error)) Report {
	if lookup == nil {
		lookup = exec.LookPath
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	report := Report{OS: runtime.GOOS, Arch: runtime.GOARCH}
	report.WSL = isWSL(readFile)
	report.Systemd = binaryFound("systemctl", lookup) && pathExists("/run/systemd/system")
	report.UserNamespaces = userNamespacesAvailable(readFile)
	for _, name := range []string{"tmux", "ssh", "rsync", providerBinary} {
		if name == "" {
			continue
		}
		path, err := lookup(name)
		report.Binaries = append(report.Binaries, BinaryCheck{Name: name, Path: path, Found: err == nil})
	}
	sort.Slice(report.Binaries, func(i, j int) bool { return report.Binaries[i].Name < report.Binaries[j].Name })
	if path, err := lookup("bwrap"); err == nil {
		report.SandboxHelper = path
	}
	switch runtime.GOOS {
	case "linux":
		report.SandboxPrimitiveReady = report.UserNamespaces && report.SandboxHelper != ""
		// A namespace primitive is necessary but not sufficient. Production
		// readonly also needs the still-unimplemented v2 inference broker to
		// bind run/session/plan/expiry/generation without exposing credentials.
		report.CredentialProjectionReady = false
		report.ReadonlyEnforce = report.SandboxPrimitiveReady && report.CredentialProjectionReady
		if !report.SandboxPrimitiveReady {
			report.Warnings = append(report.Warnings, "hard readonly isolation is not proven: user namespaces and bwrap are required")
		}
		report.Warnings = append(report.Warnings, "hard readonly is blocked until the credential-free provider broker v2 is deployed and validated")
		if report.WSL && !report.Systemd {
			report.Warnings = append(report.Warnings, "WSL is running without systemd; write-mode supervision is unavailable")
		}
	case "darwin":
		if path, err := lookup("sandbox-exec"); err == nil {
			report.SandboxHelper = path
		}
		report.Warnings = append(report.Warnings, "macOS readonly adapter is unavailable")
	default:
		report.Warnings = append(report.Warnings, "unsupported operating system")
	}
	return report
}

func binaryFound(name string, lookup func(string) (string, error)) bool {
	_, err := lookup(name)
	return err == nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isWSL(readFile func(string) ([]byte, error)) bool {
	if os.Getenv("WSL_INTEROP") != "" || os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	for _, path := range []string{"/proc/sys/kernel/osrelease", "/proc/version"} {
		data, err := readFile(path)
		if err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft") {
			return true
		}
	}
	return false
}

func userNamespacesAvailable(readFile func(string) ([]byte, error)) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	data, err := readFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) != "0"
}
