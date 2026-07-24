package transport

import "context"

// Capabilities is the subset of the Nextcloud capabilities document that
// Nimbo cares about. The full document is large; we decode only what we
// use and let the rest fall away.
type Capabilities struct {
	Version    ServerVersion
	NotifyPush *NotifyPush // nil when the notify_push app is not installed
	Files      FilesCapability
	Theming    Theming
}

// Theming is the server/user theme (from the theming app): a primary colour the
// client can accent its UI with to match the user's Nextcloud.
type Theming struct {
	Name  string `json:"name"`
	Color string `json:"color"` // primary colour, hex e.g. "#0082c9"
}

// ServerVersion identifies the running Nextcloud server.
type ServerVersion struct {
	Major  int    `json:"major"`
	Minor  int    `json:"minor"`
	Micro  int    `json:"micro"`
	String string `json:"string"`
}

// NotifyPush holds the real-time push endpoints advertised by the notify_push
// app. The websocket carries notify_file / notify_notification / notify_activity
// events; pre_auth issues a short-lived token used to authenticate the socket.
type NotifyPush struct {
	Websocket string `json:"websocket"`
	PreAuth   string `json:"pre_auth"`
}

// FilesCapability carries the server's rules about what filenames it will
// accept, so the client can refuse to upload names the server would reject
// (.htaccess, names with illegal characters, etc.) instead of failing forever.
// Field names cover both the modern (Nextcloud 30+) and legacy schemas.
type FilesCapability struct {
	ForbiddenFilenames    []string `json:"forbidden_filenames"`
	ForbiddenBasenames    []string `json:"forbidden_filename_basenames"`
	ForbiddenCharacters   []string `json:"forbidden_filename_characters"`
	ForbiddenExtensions   []string `json:"forbidden_filename_extensions"`
	BlacklistedFiles      []string `json:"blacklisted_files"` // legacy
}

// FetchCapabilities retrieves and decodes the server capabilities. A successful
// call also confirms the account's app password is accepted, making it a good
// post-login health check.
func (c *Client) FetchCapabilities(ctx context.Context) (*Capabilities, error) {
	var data struct {
		Version      ServerVersion `json:"version"`
		Capabilities struct {
			NotifyPush *struct {
				Type      []string `json:"type"`
				Endpoints struct {
					Websocket string `json:"websocket"`
					PreAuth   string `json:"pre_auth"`
				} `json:"endpoints"`
			} `json:"notify_push"`
			Files   FilesCapability `json:"files"`
			Theming Theming         `json:"theming"`
		} `json:"capabilities"`
	}
	if err := c.getOCS(ctx, "cloud/capabilities", &data); err != nil {
		return nil, err
	}

	caps := &Capabilities{Version: data.Version, Files: data.Capabilities.Files, Theming: data.Capabilities.Theming}
	if np := data.Capabilities.NotifyPush; np != nil && np.Endpoints.Websocket != "" {
		caps.NotifyPush = &NotifyPush{
			Websocket: np.Endpoints.Websocket,
			PreAuth:   np.Endpoints.PreAuth,
		}
	}
	return caps, nil
}
