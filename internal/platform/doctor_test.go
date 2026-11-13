package platform

import (
	"errors"
	"slices"
	"testing"
)

func TestDoctorDoesNotClaimReadonlyWithoutSandboxHelper(t *testing.T) {
	lookup := func(name string) (string, error) {
		if name == "bwrap" {
			return "", errors.New("missing")
		}
		return "/usr/bin/" + name, nil
	}
	read := func(path string) ([]byte, error) {
		if path == "/proc/sys/user/max_user_namespaces" {
			return []byte("1024\n"), nil
		}
		return nil, errors.New("missing")
	}
	report := Doctor("codex", lookup, read)
	if report.OS == "linux" && report.ReadonlyEnforce {
		t.Fatal("readonly must not be advertised without bwrap")
	}
}

func TestDoctorDoesNotConfuseBwrapWithProductionReadonlyReadiness(t *testing.T) {
	lookup := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	read := func(path string) ([]byte, error) {
		if path == "/proc/sys/user/max_user_namespaces" {
			return []byte("1024\n"), nil
		}
		return nil, errors.New("missing")
	}
	report := Doctor("codex", lookup, read)
	if report.OS == "linux" {
		if !report.SandboxPrimitiveReady || report.CredentialProjectionReady || report.ReadonlyEnforce {
			t.Fatalf("incorrect readonly readiness: %#v", report)
		}
		if !slices.Contains(report.Warnings, "hard readonly is blocked until the credential-free provider broker v2 is deployed and validated") {
			t.Fatalf("missing v2 blocker warning: %#v", report.Warnings)
		}
	}
}

func TestWSLDetectionFromProc(t *testing.T) {
	read := func(path string) ([]byte, error) {
		switch path {
		case "/proc/sys/kernel/osrelease":
			return []byte("6.6.87.2-microsoft-standard-WSL2"), nil
		case "/proc/sys/user/max_user_namespaces":
			return []byte("1024"), nil
		default:
			return nil, errors.New("missing")
		}
	}
	if !Doctor("codex", func(name string) (string, error) { return "/x/" + name, nil }, read).WSL {
		t.Fatal("expected WSL detection")
	}
}
