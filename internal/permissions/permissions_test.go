package permissions

import (
	"errors"
	"testing"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name              string
		confirm, readonly bool
		want              Mode
		wantErr           error
	}{
		{name: "compatibility default", want: ModeFull},
		{name: "confirm", confirm: true, want: ModeConfirm},
		{name: "readonly", readonly: true, want: ModeReadOnly},
		{name: "conflict", confirm: true, readonly: true, wantErr: ErrConflictingModes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Resolve(test.confirm, test.readonly)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Resolve error = %v, want %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("Resolve = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPolicyForCanonicalAndClosed(t *testing.T) {
	for _, mode := range []Mode{ModeFull, ModeConfirm, ModeReadOnly} {
		policy, err := PolicyFor(mode)
		if err != nil {
			t.Fatal(err)
		}
		if err := policy.Validate(); err != nil {
			t.Fatalf("%s policy invalid: %v", mode, err)
		}
		if len(policy.PolicyDigest) != 64 {
			t.Fatalf("%s digest length = %d", mode, len(policy.PolicyDigest))
		}
		if policy.AllowCredentialExposure {
			t.Fatalf("%s exposes credentials", mode)
		}
	}

	readonly, _ := PolicyFor(ModeReadOnly)
	if readonly.AllowFilesystemWrites || readonly.AllowExternalMutations || readonly.AllowDirectNetwork ||
		readonly.AllowDirectSSH || readonly.AllowGenericShell || readonly.AllowApprovalEscalation {
		t.Fatalf("readonly policy is fail-open: %+v", readonly)
	}
	if !readonly.HardReadOnly || !readonly.AllowFixedReadBroker {
		t.Fatalf("readonly policy lacks hard boundary/read broker: %+v", readonly)
	}

	confirm, _ := PolicyFor(ModeConfirm)
	if !confirm.RequirePerActionConfirmation {
		t.Fatal("confirm mode does not require per-action confirmation")
	}

	full, _ := PolicyFor(ModeFull)
	if full.WarningCode != "full_access_unsafe" {
		t.Fatalf("full warning = %q", full.WarningCode)
	}
	full.AllowCredentialExposure = true
	if err := full.Validate(); err == nil {
		t.Fatal("mutated policy accepted")
	}
}
