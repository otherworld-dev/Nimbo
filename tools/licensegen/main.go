// Command licensegen is Otherworld Dev Ltd's licence-signing tool. It generates
// the Ed25519 signing keypair (once) and issues Nimbo business licences. The
// PRIVATE key it writes must be kept secret and backed up — it is the only
// thing that can mint valid licences, and it never ships in the app.
//
//	go run ./tools/licensegen genkey                         # one-time: make the keypair
//	go run ./tools/licensegen sign -customer "Acme Ltd" \
//	        -seats 50 -expires 2027-06-14                     # issue a licence
//
// genkey writes the private key to tools/licensegen/.signing-key (gitignored)
// and prints the public-key Go literal to paste into internal/license.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/license"
)

const keyFile = "tools/licensegen/.signing-key"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: licensegen <genkey|sign> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "genkey":
		genkey()
	case "sign":
		sign(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func genkey() {
	if _, err := os.Stat(keyFile); err == nil {
		fmt.Fprintf(os.Stderr, "refusing to overwrite existing %s (delete it deliberately to rotate keys)\n", keyFile)
		os.Exit(1)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		panic(err)
	}
	// Print the public key as a Go byte literal for internal/license.
	var b strings.Builder
	b.WriteString("var SigningPublicKey = ed25519.PublicKey{\n")
	for i, by := range pub {
		if i%12 == 0 {
			b.WriteString("\t")
		}
		fmt.Fprintf(&b, "0x%02x, ", by)
		if i%12 == 11 {
			b.WriteString("\n")
		}
	}
	b.WriteString("\n}")
	fmt.Printf("Wrote private key to %s (KEEP SECRET, BACK UP).\n\n", keyFile)
	fmt.Println("Paste this into internal/license/license.go (replacing SigningPublicKey):")
	fmt.Println()
	fmt.Println(b.String())
}

func sign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	customer := fs.String("customer", "", "customer / organisation name (required)")
	tier := fs.String("tier", "business", "licence tier")
	seats := fs.Int("seats", 0, "licensed seat count (0 = unspecified)")
	expires := fs.String("expires", "", "expiry YYYY-MM-DD (empty = perpetual)")
	id := fs.String("id", "", "licence id (default: generated)")
	_ = fs.Parse(args)
	if *customer == "" {
		fmt.Fprintln(os.Stderr, "sign: -customer is required")
		os.Exit(2)
	}
	if *expires != "" {
		if _, err := time.Parse("2006-01-02", *expires); err != nil {
			fmt.Fprintln(os.Stderr, "sign: -expires must be YYYY-MM-DD")
			os.Exit(2)
		}
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign: cannot read %s — run 'licensegen genkey' first: %v\n", keyFile, err)
		os.Exit(1)
	}
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		fmt.Fprintln(os.Stderr, "sign: signing key is corrupt")
		os.Exit(1)
	}
	licID := *id
	if licID == "" {
		var r [6]byte
		_, _ = rand.Read(r[:])
		licID = "NL-" + base64.RawURLEncoding.EncodeToString(r[:])
	}
	claims := license.Claims{
		Customer: *customer,
		Tier:     license.Tier(*tier),
		Seats:    *seats,
		Issued:   time.Now().UTC().Format("2006-01-02"),
		Expires:  *expires,
		ID:       licID,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	sig := ed25519.Sign(ed25519.PrivateKey(priv), payload)
	token := "NIMBO-LIC-1." +
		base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig)

	fmt.Printf("Issued %s licence to %q (id %s", claims.Tier, claims.Customer, claims.ID)
	if claims.Expires != "" {
		fmt.Printf(", expires %s", claims.Expires)
	}
	fmt.Println("):")
	fmt.Println()
	fmt.Println(token)
}
