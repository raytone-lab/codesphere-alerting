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
		if stats.Disabled {
			logger.Info("balancealert cron skipped: globally disabled")
			return
		}
		logger.Infof("balancealert cron: prepaid=%d skipped=%d sent=%d cooldown=%d failed=%d",
			stats.Prepaid, stats.SkippedIncome, stats.Sent, stats.Cooldown, stats.Failed)
	})
	if err != nil {
		logger.Errorf("balancealert cron schedule: %v", err)
		return
	}

	_, err = c.AddFunc("0 10 * * *", func() {
		stats, err := RunOnceDynamic(n9e, nil)
		if err != nil {
			logger.Errorf("balancealert dynamic cron: %v", err)
			return
		}
		if stats.Disabled {
			logger.Info("balancealert dynamic cron skipped: globally disabled")
			return
		}
		logger.Infof("balancealert dynamic cron: prepaid=%d evaluated=%d sent=%d cooldown=%d failed=%d",
			stats.Prepaid, stats.Evaluated, stats.Sent, stats.Cooldown, stats.Failed)
	})
	if err != nil {
		logger.Errorf("balancealert dynamic cron schedule: %v", err)
		return
	}

	_, err = c.AddFunc("5 8 * * *", func() {
		res, err := ReconMissReports(n9e, 7)
		if err != nil {
			logger.Errorf("balancealert miss-report cron: %v", err)
			return
		}
		if res.Disabled {
			logger.Info("balancealert miss-report cron skipped: globally disabled")
			return
		}
		logger.Infof("balancealert miss-report cron: scanned=%d misses=%d", res.PrepaidScanned, len(res.Misses))
		for _, m := range res.Misses {
			logger.Warningf("balancealert miss-report: account=%s name=%s balance=%.2f reason=%s",
				m.BillingAccountID, m.Name, m.BalanceUSD, m.Reason)
		}
	})
	if err != nil {
		logger.Errorf("balancealert miss-report cron schedule: %v", err)
		return
	}

	c.Start()
	logger.Info("balancealert cron started (static: every 15 minutes, dynamic: daily at 10:00, miss-report: daily at 08:05)")
}
