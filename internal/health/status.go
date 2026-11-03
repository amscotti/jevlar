package health

import "time"

// KeyStatus is the externally visible state of one provider key, used by
// /admin/status. It never contains secret material.
type KeyStatus struct {
	ID                  string `json:"id"`
	State               State  `json:"state"`
	Reason              string `json:"reason,omitempty"`
	CooldownUntil       string `json:"cooldown_until,omitempty"`
	Inflight            int64  `json:"inflight"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// Status returns the key's status snapshot at the registry clock.
func (k *Key) Status() KeyStatus {
	st := KeyStatus{
		ID:                  k.id,
		State:               k.State(),
		Inflight:            k.Inflight(),
		ConsecutiveFailures: int(k.consecutive.Load()),
	}
	if st.State == StateDisabled {
		st.Reason = k.DisabledReason()
	}
	if until := k.CooldownUntil(); !until.IsZero() && k.now().Before(until) {
		st.CooldownUntil = until.UTC().Format(time.RFC3339)
	}
	return st
}

// BreakerStatus is the externally visible state of one provider breaker.
type BreakerStatus struct {
	Provider            string `json:"provider"`
	State               State  `json:"state"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// Status returns the breaker's status snapshot.
func (b *Breaker) Status(provider string) BreakerStatus {
	return BreakerStatus{
		Provider:            provider,
		State:               b.State(),
		ConsecutiveFailures: b.ConsecutiveFailures(),
	}
}

// AccountStatus is the externally visible state of one account.
// TokensLeft is -1 when the account has no token budget (unlimited).
type AccountStatus struct {
	Name          string  `json:"name"`
	Cooling       bool    `json:"cooling"`
	CooldownUntil string  `json:"cooldown_until,omitempty"`
	TokensLeft    float64 `json:"tokens_left"`
}

// Status returns the account's status snapshot.
func (a *Account) Status() AccountStatus {
	cooling, until := a.Cooling()
	st := AccountStatus{Name: a.name, Cooling: cooling, TokensLeft: a.TokensLeft()}
	if cooling {
		st.CooldownUntil = until.UTC().Format(time.RFC3339)
	}
	return st
}

// ClientStatus is the externally visible state of one inbound client.
// TokensLeft is -1 when the client has no token budget (unlimited).
type ClientStatus struct {
	ID           string  `json:"id"`
	Limited      bool    `json:"limited"`
	LimitedUntil string  `json:"limited_until,omitempty"`
	TokensLeft   float64 `json:"tokens_left"`
}

// Status returns the client's status snapshot.
func (c *Client) Status() ClientStatus {
	limited, until := c.LimitedUntil()
	st := ClientStatus{ID: c.id, Limited: limited, TokensLeft: c.TokensLeft()}
	if limited {
		st.LimitedUntil = until.UTC().Format(time.RFC3339)
	}
	return st
}
