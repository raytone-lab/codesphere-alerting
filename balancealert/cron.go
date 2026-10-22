package balancealert

import (
	"github.com/ccfos/nightingale/v6/models"
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

	// 首次升级到名单版本时物化表还是空的，15 分钟轮次会 fail closed 到次日
	// 07:30。启动即补一次，避免上线后整整一天不发预警。已有成功记录则不做事。
	go func() {
		state, err := models.BalanceAlertExcludeStateGet(n9e)
		if err != nil || state.Ready() {
			return
		}
		if _, err := RefreshCreditExcludes(n9e, "STARTUP"); err != nil {
			logger.Errorf("balancealert exclude refresh startup: %v", err)
		}
	}()

	// 排除名单每日 07:30（Asia/Shanghai）全量刷新。独立调度器显式绑定时区：
	// 不能用默认调度器的进程本地时区，否则容器跑 UTC 时会在北京时间 15:30 才刷。
	refreshCron := cron.New(cron.WithLocation(locShanghai))
	_, err = refreshCron.AddFunc("30 7 * * *", func() {
		if _, err := RefreshCreditExcludes(n9e, "CRON"); err != nil {
			logger.Errorf("balancealert exclude refresh cron: %v", err)
		}
	})
	if err != nil {
		logger.Errorf("balancealert exclude refresh cron schedule: %v", err)
		return
	}
	refreshCron.Start()

	c.Start()
	logger.Info("balancealert cron started (static: every 15 minutes, dynamic: daily at 10:00, miss-report: daily at 08:05, credit-exclude refresh: daily at 07:30 Asia/Shanghai)")
}
