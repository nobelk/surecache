// SPDX-License-Identifier: Apache-2.0

package surecachepg

import (
	"net"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ConfigurePool applies b's connection bounds to cfg (design section 4.6): it sets
// idle_in_transaction_session_timeout as a connection parameter, so an abandoned
// transaction cannot hold the shared fence forever, and installs a dialer with TCP
// keepalive, so a dead peer is detected. It changes only cfg; the caller owns the pool.
func ConfigurePool(cfg *pgxpool.Config, b Bounds) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = strconv.FormatInt(millis(b.IdleInTxTimeout), 10) + "ms"
	dialer := &net.Dialer{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     b.KeepAlive.Idle,
			Interval: b.KeepAlive.Interval,
			Count:    b.KeepAlive.Count,
		},
	}
	cfg.ConnConfig.DialFunc = dialer.DialContext
	return nil
}
