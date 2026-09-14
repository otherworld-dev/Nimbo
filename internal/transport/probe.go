package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TLSProbe is what one bare TLS handshake with a candidate local address showed.
type TLSProbe struct {
	Verified    bool   // chain + name check passed against the trust store
	Pinned      bool   // the leaf matched the pin argument
	PlainHTTP   bool   // the endpoint answered with HTTP, not TLS
	Fingerprint string // lowercase hex SHA-256 of the leaf
	Subject     string
	Issuer      string
	NotAfter    time.Time
	VerifyError string // why Verified is false ("" when true)
}

// ProbeTLS opens one TLS connection to addr (host:port), presenting serverName
// as SNI, and closes it straight after the handshake: no HTTP request, no
// credentials. It captures the leaf and verifies it by hand, so one handshake
// yields both the verdict and the details the trust dialog shows. The verdict
// is reported, not enforced — nothing is ever sent on this connection — which
// is why InsecureSkipVerify is acceptable here and nowhere else unpinned.
func ProbeTLS(ctx context.Context, addr, serverName, pin string) (TLSProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, localDialTimeout+localHandshakeTimeout)
	defer cancel()
	var p TLSProbe
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: localDialTimeout},
		Config: &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: true, // see VerifyConnection + the doc comment
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("no certificate presented")
				}
				leaf := cs.PeerCertificates[0]
				p.Fingerprint = Fingerprint(leaf)
				p.Subject = leaf.Subject.String()
				p.Issuer = leaf.Issuer.String()
				p.NotAfter = leaf.NotAfter
				p.Pinned = pin != "" && strings.EqualFold(pin, p.Fingerprint)
				inter := x509.NewCertPool()
				for _, c := range cs.PeerCertificates[1:] {
					inter.AddCert(c)
				}
				_, err := leaf.Verify(x509.VerifyOptions{DNSName: serverName, Intermediates: inter, Roots: localRootCAs})
				if err != nil {
					p.VerifyError = err.Error()
				} else {
					p.Verified = true
				}
				return nil
			},
		},
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		var rhe tls.RecordHeaderError
		if errors.As(err, &rhe) {
			p.PlainHTTP = true
			return p, nil
		}
		return p, err
	}
	conn.Close()
	return p, nil
}

// NextcloudProbe is what one unauthenticated GET of status.php over a
// candidate local address showed.
type NextcloudProbe struct {
	Nextcloud bool   // the address answered status.php like a Nextcloud instance
	Product   string // productname ("Nextcloud")
	Version   string // versionstring ("33.0.0")
	Detail    string // why Nextcloud is false: the HTTP status, or what came back instead
}

// ProbeNextcloud asks addr for status.php shaped exactly like a real
// local-route request — dial addr, SNI and Host = the account's public host,
// the public URL's path — pinned to the leaf fingerprint ProbeTLS just showed
// so nothing else can answer, and with NO credentials: status.php is public,
// which is what makes it safe to ask before the user has trusted the
// certificate. It tells a wrong host (a router or NAS admin page with its own
// certificate) apart from a Nextcloud; it cannot tell whose Nextcloud (its
// fields are trivially faked), which is why the oc:id comparison still runs
// after Trust. err is a connection-level failure (dial, TLS, pin); an answer
// that isn't a Nextcloud is reported in the value, not as an error.
func ProbeNextcloud(ctx context.Context, server, addr, pin string) (NextcloudProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, localDialTimeout+localHandshakeTimeout+5*time.Second)
	defer cancel()
	tr := newLocalTransport(addr, pin)
	defer tr.CloseIdleConnections()
	hc := &http.Client{
		Transport: tr,
		// A redirect would carry the request to whatever host it names; the
		// answer wanted is at this address or nowhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(server, "/")+"/status.php", nil)
	if err != nil {
		return NextcloudProbe{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return NextcloudProbe{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return NextcloudProbe{Detail: "status.php answered HTTP " + strconv.Itoa(resp.StatusCode)}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return NextcloudProbe{}, err
	}
	var st struct {
		Installed     *bool  `json:"installed"`
		ProductName   string `json:"productname"`
		VersionString string `json:"versionstring"`
		Version       string `json:"version"`
	}
	if err := json.Unmarshal(body, &st); err != nil || st.Installed == nil {
		return NextcloudProbe{Detail: "status.php isn't a Nextcloud status page"}, nil
	}
	p := NextcloudProbe{Nextcloud: true, Product: st.ProductName, Version: st.VersionString}
	if p.Product == "" {
		p.Product = "Nextcloud"
	}
	if p.Version == "" {
		p.Version = st.Version
	}
	return p, nil
}
