package health

import (
	"sync"
	"time"
)

// refillWindow is the window a bucket's budget is expressed over: rpm and
// tpm are per minute.
const refillWindow = time.Minute

// bucket is a continuously-refilling token bucket measured in tokens per
// minute. It is deliberately simple: capacity equals one minute's budget
// and tokens refill at budget/window per second, so a burst up to the
// full minute allowance is possible and the long-run rate is exact.
type bucket struct {
	mu       sync.Mutex
	capacity float64
	rate     float64 // tokens per second
	tokens   float64
	last     time.Time
	now      func() time.Time
}

func newBucket(capacity int, now func() time.Time) *bucket {
	return &bucket{
		capacity: float64(capacity),
		rate:     float64(capacity) / refillWindow.Seconds(),
		tokens:   float64(capacity),
		last:     now(),
		now:      now,
	}
}

// configure applies changed limits. State is preserved while the limits
// are unchanged, so a reload with identical quotas does not hand out a
// fresh minute; raising a budget grants the increase immediately (an
// operator who raises a quota expects it to take effect now), while
// lowering it caps the balance at the new capacity.
func (b *bucket) configure(capacity int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	old := b.capacity
	if float64(capacity) == old {
		return
	}
	b.capacity = float64(capacity)
	b.rate = float64(capacity) / refillWindow.Seconds()
	if capacity == 0 {
		b.tokens = 0
		return
	}
	if b.capacity > old {
		b.tokens += b.capacity - old
	}
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
}

func (b *bucket) refill() {
	now := b.now()
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		// A clock that stood still or moved backwards must not move the
		// refill anchor backwards, which would refill the same span twice.
		return
	}
	b.tokens += elapsed.Seconds() * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
}

// take consumes n tokens, reporting whether they were available and, when
// they were not, how long until enough have accrued.
func (b *bucket) take(n float64) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capacity <= 0 {
		return true, 0 // unlimited
	}
	b.refill()
	if b.tokens >= n {
		b.tokens -= n
		return true, 0
	}
	missing := n - b.tokens
	if b.rate <= 0 {
		return false, refillWindow
	}
	return false, time.Duration(missing / b.rate * float64(time.Second))
}

// tokensLeft reports the current balance, for status output; -1 means the
// budget is unlimited.
func (b *bucket) tokensLeft() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capacity <= 0 {
		return -1
	}
	b.refill()
	return b.tokens
}

// Account is the process-lifetime limit state of one provider account.
// Provider limits apply to the account, not to individual keys: a 429 on
// one key cools every key in the account.
type Account struct {
	name string

	mu     sync.Mutex
	rpm    *bucket
	tpm    *bucket
	cooled int64 // Unix nanoseconds
	now    func() time.Time
}

func newAccount(rpm, tpm int, now func() time.Time) *Account {
	return &Account{
		rpm: newBucket(rpm, now),
		tpm: newBucket(tpm, now),
		now: now,
	}
}

func (a *Account) configure(rpm, tpm int) {
	a.rpm.configure(rpm)
	a.tpm.configure(tpm)
}

// Name returns the account name.
func (a *Account) Name() string { return a.name }

// TakeRequest consumes one request from the account's rpm budget. It
// reports whether the request may proceed and, when not, the wait.
func (a *Account) TakeRequest() (bool, time.Duration) {
	return a.rpm.take(1)
}

// TakeTokens consumes tokens from the account's tpm budget after a
// response has reported its usage.
func (a *Account) TakeTokens(n int) (bool, time.Duration) {
	if n <= 0 {
		return true, 0
	}
	return a.tpm.take(float64(n))
}

// CoolFor puts the whole account into cooldown until at least now+d.
func (a *Account) CoolFor(d time.Duration) {
	if d <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	until := a.now().Add(d).UnixNano()
	if until > a.cooled {
		a.cooled = until
	}
}

// Cooling reports whether the account is cooling down and until when.
func (a *Account) Cooling() (bool, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	until := a.cooled
	if until == 0 {
		return false, time.Time{}
	}
	t := time.Unix(0, until)
	return a.now().Before(t), t
}

// CooldownUntil returns the account cooldown deadline, or the zero time.
func (a *Account) CooldownUntil() time.Time {
	_, until := a.Cooling()
	return until
}

// TokensLeft reports the account's remaining token budget for status, or
// -1 when the budget is unlimited.
func (a *Account) TokensLeft() float64 { return a.tpm.tokensLeft() }

// Client is the process-lifetime quota state of one inbound client.
type Client struct {
	id string

	rpm *bucket
	tpm *bucket

	mu        sync.Mutex
	limitedTo int64 // Unix nanoseconds; tpm post-hoc limit
	now       func() time.Time
}

func newClient(rpm, tpm int, now func() time.Time) *Client {
	return &Client{
		rpm: newBucket(rpm, now),
		tpm: newBucket(tpm, now),
		now: now,
	}
}

func (c *Client) configure(rpm, tpm int) {
	c.rpm.configure(rpm)
	c.tpm.configure(tpm)
}

// ID returns the client id.
func (c *Client) ID() string { return c.id }

// TakeRequest consumes one request from the client's rpm budget.
func (c *Client) TakeRequest() (bool, time.Duration) {
	return c.rpm.take(1)
}

// TakeTokens consumes tokens from the client's tpm budget.
func (c *Client) TakeTokens(n int) (bool, time.Duration) {
	if n <= 0 {
		return true, 0
	}
	return c.tpm.take(float64(n))
}

// LimitFor records a post-hoc tpm overrun: the client is refused until
// the given deadline because it has already spent its minute's tokens.
func (c *Client) LimitFor(until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u := until.UnixNano(); u > c.limitedTo {
		c.limitedTo = u
	}
}

// LimitedUntil reports whether the client is currently limited and until
// when.
func (c *Client) LimitedUntil() (bool, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limitedTo == 0 {
		return false, time.Time{}
	}
	t := time.Unix(0, c.limitedTo)
	return c.now().Before(t), t
}

// TokensLeft reports the client's remaining token budget, or -1 when the
// budget is unlimited.
func (c *Client) TokensLeft() float64 { return c.tpm.tokensLeft() }
