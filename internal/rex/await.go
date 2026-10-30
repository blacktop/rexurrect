package rex

import (
	"context"
	"errors"
	"time"
)

const (
	awaitPollInterval = 250 * time.Millisecond
	awaitCallTimeout  = 5 * time.Second
)

// AwaitServer polls whoami until the configured server answers, wait ends, or
// keepWaiting reports false because the process that started us is gone. It
// returns the answering server's PID; a server that never answers yields 0
// and no error, so the caller treats it as a benign skip, while a malformed
// answer is an error.
func (c Client) AwaitServer(ctx context.Context, wait time.Duration, keepWaiting func() bool) (int, error) {
	if wait <= 0 {
		return 0, errors.New("the server wait must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	c.CallTimeout = min(c.CallTimeout, awaitCallTimeout)
	if c.CallTimeout <= 0 {
		c.CallTimeout = awaitCallTimeout
	}
	for keepWaiting() && ctx.Err() == nil {
		id, err := c.whoami(ctx)
		if err == nil {
			if !keepWaiting() || ctx.Err() != nil {
				return 0, nil
			}
			if id.PID <= 0 {
				return 0, errors.New("the Rex server reported no PID")
			}
			return id.PID, nil
		}
		pause(ctx, awaitPollInterval)
	}
	return 0, nil
}
