package account

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ParseLocalAddress validates a user-typed local network address and returns
// it in canonical form: lowercase host, an IPv6 literal in brackets, an
// explicit port kept as typed and no port otherwise (the transport fills in
// the account URL's port). publicHost is the account URL's hostname; typing
// that is refused — it would only resolve through DNS like today.
func ParseLocalAddress(s, publicHost string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("enter an address")
	}
	if strings.Contains(s, "://") {
		return "", errors.New("enter a host name or IP address, not a URL")
	}
	if strings.ContainsAny(s, "/?#") {
		return "", errors.New("enter a host name or IP address without a path")
	}
	host, port := s, ""
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	} else if ip := net.ParseIP(strings.Trim(s, "[]")); ip != nil && ip.To4() == nil {
		host = ip.String() // bare IPv6 literal, no port
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return "", errors.New("enter an address")
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "", errors.New("enter a valid host name or IP address")
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid port %q", port)
		}
	}
	if host == strings.ToLower(publicHost) {
		return "", errors.New("that is the public address — enter the server's local network address instead")
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		return host + ":" + port, nil
	}
	return host, nil
}
