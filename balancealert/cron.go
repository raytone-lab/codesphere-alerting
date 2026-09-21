package balancealert

import (
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/robfig/cron/v3"
	"github.com/toolkits/pkg/logger"
)

func StartCron(n9e *ctx.Context) {
	c := cron.New()
	_, err := c.AddFunc("*/15 * * * *", func() {
		stats, err := RunOnce(n9e, nil)
		if err != nil {
			logger.Errorf("balancealert cron: %v", err)
			return
		}
		logger.Infof("balancealert cron: prepaid=%d skipped=%d sent=%d cooldown=%d failed=%d",
			stats.Prepaid, stats.SkippedIncome, stats.Sent, stats.Cooldown, stats.Failed)
	})
	if err != nil {
		logger.Errorf("balancealert cron schedule: %v", err)
		return
	}
	c.Start()
	logger.Info("balancealert cron started (every 15 minutes)")
}
