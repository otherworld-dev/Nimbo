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
}
