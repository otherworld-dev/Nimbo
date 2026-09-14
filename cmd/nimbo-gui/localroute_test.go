package main

import (
	"os"
	"strings"
	"testing"
)

// The local-address Test flow has a fixed order that two different lessons
// pin: nothing carrying credentials may go to the address before its
// certificate is trusted (the oc:id comparison comes after the trust gate),
// and — found live 2026-09-14 when a wrong LAN host with its own certificate
// drew the trust dialog — the credential-free status.php check must run
// BEFORE that dialog, so a router or NAS admin page is reported as "not a
// Nextcloud" instead of the user being asked to trust its certificate.
//
// Source-inspection test: testLocalAddress is GUI glue that needs a signed-in
// engine, so the ordering is asserted on the code itself.
func TestLocalAddressChecksNextcloudBeforeTrustAndCredentialsAfter(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (a *App) testLocalAddress(")
	if start < 0 {
		t.Fatal("testLocalAddress not found")
	}
	end := strings.Index(body[start:], "\n}")
	if end < 0 {
		t.Fatal("testLocalAddress end not found")
	}
	fn := body[start : start+end]

	probe := strings.Index(fn, "transport.ProbeNextcloud(")
	untrusted := strings.Index(fn, `dto.Result = "untrusted"`)
	creds := strings.Index(fn, "eng.CheckLocalRoute(")
	switch {
	case probe < 0:
		t.Error("the untrusted branch never asks status.php — a wrong host with a self-signed certificate gets the trust dialog")
	case untrusted < 0:
		t.Error(`no "untrusted" verdict — the trust gate is gone`)
	case probe > untrusted:
		t.Error("status.php is checked AFTER the trust verdict — it must come before the dialog is offered")
	}
	if creds >= 0 && untrusted >= 0 && creds < untrusted {
		t.Error("the credentialed oc:id comparison runs before the trust gate — credentials must never reach an untrusted address")
	}
}
