package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Login Flow v2 is Nextcloud's recommended interactive auth: we ask the server
// to start a flow, send the user to a browser URL to approve, then poll until
// the server hands back a dedicated *app password* (revocable, never the user's
// real password). See:
// https://docs.nextcloud.com/server/latest/developer_manual/client_apis/LoginFlow/index.html

// Flow is an in-progress Login Flow v2 session.
type Flow struct {
	// LoginURL is the page the user must open in a browser to approve the login.
	LoginURL string

	pollToken    string
	pollEndpoint string
	hc           *http.Client
	pollInterval time.Duration // between poll attempts; 0 means the 2s default
}

// normalizeServerURL makes user-typed server addresses usable: trims
// whitespace, defaults the scheme to https, and drops any trailing slash.
func normalizeServerURL(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return strings.TrimRight(s, "/")
}

// Credentials are the result of a completed login flow.
type Credentials struct {
	Server      string `json:"server"`
	LoginName   string `json:"loginName"`
	AppPassword string `json:"appPassword"`
}

// userAgent is sent as the device name; it appears in the user's
// Settings → Security → Devices & sessions list next to the app password.
const userAgent = "Nimbo"

// InitLogin starts a Login Flow v2 against serverURL and returns a Flow whose
// LoginURL the caller should open in a browser.
func InitLogin(ctx context.Context, serverURL string) (*Flow, error) {
	serverURL = normalizeServerURL(serverURL)
	endpoint := serverURL + "/index.php/login/v2"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("start login flow: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("start login flow: server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var out struct {
		Poll struct {
			Token    string `json:"token"`
			Endpoint string `json:"endpoint"`
		} `json:"poll"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode login flow response: %w", err)
	}
	if out.Login == "" || out.Poll.Token == "" || out.Poll.Endpoint == "" {
		return nil, fmt.Errorf("start login flow: incomplete response from server")
	}

	return &Flow{
		LoginURL:     out.Login,
		pollToken:    out.Poll.Token,
		pollEndpoint: out.Poll.Endpoint,
		hc:           hc,
	}, nil
}

// errTransientPoll marks poll failures worth retrying: the server-side flow
// token stays valid across a dropped request, so one network hiccup (a
// Wi-Fi→cellular handover while the user approves in the browser) must not
// kill the login.
var errTransientPoll = errors.New("transient poll failure")

// Poll blocks until the user completes the browser login, the context is
// cancelled, or the server reports the flow has expired. While pending, the
// server replies 404; on success it replies 200 with the credentials.
// Transient transport errors are retried until the context expires.
func (f *Flow) Poll(ctx context.Context) (Credentials, error) {
	interval := f.pollInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		creds, done, err := f.pollOnce(ctx)
		if err != nil && (ctx.Err() != nil || !errors.Is(err, errTransientPoll)) {
			return Credentials{}, err
		}
		if done {
			return creds, nil
		}
		select {
		case <-ctx.Done():
			return Credentials{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// pollOnce performs a single poll. done is true only when credentials are
// returned; a pending flow yields done=false with a nil error.
func (f *Flow) pollOnce(ctx context.Context) (creds Credentials, done bool, err error) {
	form := url.Values{"token": {f.pollToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.pollEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	resp, err := f.hc.Do(req)
	if err != nil {
		return Credentials{}, false, fmt.Errorf("poll login flow: %w: %w", errTransientPoll, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
			return Credentials{}, false, fmt.Errorf("decode credentials: %w", err)
		}
		if creds.AppPassword == "" || creds.LoginName == "" {
			return Credentials{}, false, fmt.Errorf("login flow returned incomplete credentials")
		}
		return creds, true, nil
	case http.StatusNotFound:
		// Flow not yet approved — keep waiting.
		return Credentials{}, false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return Credentials{}, false, fmt.Errorf("poll login flow: server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
}

// Complete persists the credentials from a finished flow: it derives a stable
// account ID, stores the app password in the OS keychain, and records the
// account in the store. It returns the saved account.
func Complete(store *Store, creds Credentials) (Account, error) {
	server := strings.TrimRight(creds.Server, "/")
	a := Account{
		ID:        newID(server, creds.LoginName),
		ServerURL: server,
		LoginName: creds.LoginName,
	}
	if err := store.ensureDir(); err != nil {
		return Account{}, err
	}
	if err := SaveSecret(a.ID, creds.AppPassword); err != nil {
		return Account{}, err
	}
	if err := store.Upsert(a); err != nil {
		// Roll back the secret so we don't leave an orphan keychain entry.
		_ = DeleteSecret(a.ID)
		return Account{}, err
	}
	// A fresh sign-in becomes the active account — both on first run and when
	// adding a second account (the natural "switch to what I just added").
	if err := store.SetDefault(a.ID); err != nil {
		return Account{}, err
	}
	return a, nil
}
