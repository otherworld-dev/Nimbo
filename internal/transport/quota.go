package transport

import "context"

// QuotaInfo is the signed-in user's storage usage, from the OCS provisioning
// self endpoint. Total/Free reflect the effective quota (or the filesystem when
// no quota is set); Limit is the configured quota in bytes (< 0 = unlimited).
type QuotaInfo struct {
	Free     int64   `json:"free"`
	Used     int64   `json:"used"`
	Total    int64   `json:"total"`
	Relative float64 `json:"relative"` // percent used (0–100)
	Limit    int64   `json:"quota"`    // configured quota; -3 = default/unlimited
}

// UserQuota fetches the current user's storage usage via OCS cloud/user.
func (c *Client) UserQuota(ctx context.Context) (QuotaInfo, error) {
	var out struct {
		Quota QuotaInfo `json:"quota"`
	}
	if err := c.getOCS(ctx, "cloud/user", &out); err != nil {
		return QuotaInfo{}, err
	}
	return out.Quota, nil
}
