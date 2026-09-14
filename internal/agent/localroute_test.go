package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/otherworld/nimbo/internal/account"
)

func TestCheckLocalRouteRefusesHTTPAccount(t *testing.T) {
	e := &Engine{Account: account.Account{ServerURL: "http://cloud.example.com", LoginName: "alice"}, secret: "pw"}
	if _, err := e.CheckLocalRoute(context.Background(), "192.168.1.100", ""); err == nil {
		t.Fatal("an http:// account must be refused")
	}
}

func TestRouteSwitchMessage(t *testing.T) {
	if got := routeSwitchMessage("local", "", "192.168.1.100:443"); got != "sync route: local (192.168.1.100:443)" {
		t.Errorf("got %q", got)
	}
	if got := routeSwitchMessage("public", "certificate changed", "192.168.1.100:443"); got != "sync route: public (certificate changed)" {
		t.Errorf("got %q", got)
	}
	if !errors.Is(ErrDifferentServer, ErrDifferentServer) {
		t.Fatal("sentinel must be comparable")
	}
}
