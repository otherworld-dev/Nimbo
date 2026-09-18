//go:build windows

package cfapi

import (
	"testing"
	"unsafe"
)

// Layout pins for structs whose Go mirrors have already bitten us twice (#569:
// four missing padding bytes in CF_OPERATION_PARAMETERS; #580 investigation:
// a spurious pad before CF_SYNC_REGISTRATION.ProviderId). Offsets are the
// amd64 values from cfapi.h 10.0.26100, verified by compiling the header
// (2026-08-18). If one of these fails, the Go struct drifted — fix the struct,
// never the expectation.
func TestStructLayoutsMatchCfapiH(t *testing.T) {
	var reg syncRegistration
	if got := unsafe.Offsetof(reg.ProviderID); got != 52 {
		t.Errorf("syncRegistration.ProviderID offset = %d, want 52 (GUID is 4-aligned; no pad after FileIdentityLength)", got)
	}
	if got := unsafe.Sizeof(reg); got != 72 {
		t.Errorf("syncRegistration size = %d, want 72", got)
	}

	var op operationInfo
	if got := unsafe.Offsetof(op.CorrelationVector); got != 24 {
		t.Errorf("operationInfo.CorrelationVector offset = %d, want 24", got)
	}
	if got := unsafe.Offsetof(op.SyncStatus); got != 32 {
		t.Errorf("operationInfo.SyncStatus offset = %d, want 32", got)
	}
	if got := unsafe.Offsetof(op.RequestKey); got != 40 {
		t.Errorf("operationInfo.RequestKey offset = %d, want 40 (order is CorrelationVector, SyncStatus, RequestKey)", got)
	}
	if got := unsafe.Sizeof(op); got != 48 {
		t.Errorf("operationInfo size = %d, want 48", got)
	}

	var psi placeholderStandardInfo
	if got := unsafe.Offsetof(psi.PinState); got != 32 {
		t.Errorf("placeholderStandardInfo.PinState offset = %d, want 32", got)
	}
	if got := unsafe.Offsetof(psi.FileIdentityLength); got != 56 {
		t.Errorf("placeholderStandardInfo.FileIdentityLength offset = %d, want 56", got)
	}
	if got := unsafe.Offsetof(psi.FileIdentity); got != 60 {
		t.Errorf("placeholderStandardInfo.FileIdentity offset = %d, want 60", got)
	}
	if got := unsafe.Sizeof(psi); got != 64 {
		t.Errorf("placeholderStandardInfo size = %d, want 64", got)
	}

	var rc callbackParamsRenameCompletion
	// Split into two ifs (rather than got != 16 || got != cpRenameSourcePath)
	// because `go vet`'s bools check flags that literal form as a suspect
	// "x != c1 || x != c2" typo; behaviour is identical.
	if got := unsafe.Offsetof(rc.SourcePath); got != 16 {
		t.Errorf("callbackParamsRenameCompletion.SourcePath offset = %d, want 16 (== cpRenameSourcePath)", got)
	} else if got != cpRenameSourcePath {
		t.Errorf("callbackParamsRenameCompletion.SourcePath offset = %d, want 16 (== cpRenameSourcePath)", got)
	}
	// CANCEL_FETCH_DATA's parameters nest a second union inside Cancel:
	// { CF_CALLBACK_CANCEL_FLAGS Flags; union { struct { LARGE_INTEGER
	// FileOffset; LARGE_INTEGER Length; } FetchData; }; } — so Flags sits at 8
	// (after ParamSize + the outer union's 4 pad bytes) and the range at 16/24,
	// the same offsets FETCH_DATA's RequiredFileOffset/RequiredLength use.
	// cfapi.h 10.0.26100 lines 397-421.
	var cf callbackParamsCancelFetchData
	if got := unsafe.Offsetof(cf.Flags); got != 8 {
		t.Errorf("callbackParamsCancelFetchData.Flags offset = %d, want 8", got)
	} else if got != cpCancelFlags {
		t.Errorf("callbackParamsCancelFetchData.Flags offset = %d, want 8 (== cpCancelFlags)", got)
	}
	if got := unsafe.Offsetof(cf.FileOffset); got != 16 {
		t.Errorf("callbackParamsCancelFetchData.FileOffset offset = %d, want 16", got)
	} else if got != cpCancelOffset {
		t.Errorf("callbackParamsCancelFetchData.FileOffset offset = %d, want 16 (== cpCancelOffset)", got)
	}
	if got := unsafe.Offsetof(cf.Length); got != 24 {
		t.Errorf("callbackParamsCancelFetchData.Length offset = %d, want 24", got)
	} else if got != cpCancelLength {
		t.Errorf("callbackParamsCancelFetchData.Length offset = %d, want 24 (== cpCancelLength)", got)
	}

	var reg2 callbackRegistration
	if got := unsafe.Offsetof(reg2.Callback); got != 8 || unsafe.Sizeof(reg2) != 16 {
		t.Errorf("callbackRegistration: Callback offset %d size %d, want 8 / 16", got, unsafe.Sizeof(reg2))
	}
}
