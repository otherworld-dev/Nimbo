package config

import (
	"testing"
)

// The list is per account, like the pairs it refers to.
func TestDetachedIsPerAccount(t *testing.T) {
	base := Dirs{Config: t.TempDir(), Data: t.TempDir()}
	a, b := base.WithAccount("a"), base.WithAccount("b")
	if err := a.AddDetached(Detached{LocalDir: `C:\A`, Rel: "Team"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.LoadDetached(); len(got) != 0 {
		t.Errorf("account b sees account a's entry: %+v", got)
	}
}
