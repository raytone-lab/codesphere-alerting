package balancealert

import (
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/robfig/cron/v3"
	"github.com/toolkits/pkg/logger"
)

func StartCron(n9e *ctx.Context) {
	// 首次升级到名单版本时物化表还是空的，15 分钟轮次会 fail closed 到次日
	// 07:30。启动即补一次，并且放在注册任何 cron 之前**同步**完成：补刷若异步，
	// 进程恰好在 xx:59:5x 启动时首轮判定会先于补刷执行并静默停圈。
	// 补刷失败不阻断启动（n9e-center 同时承载全部告警能力）：记录并告警后继续
	// 注册 cron，当轮按 fail closed 处理，等下一次定时刷新。
	refreshOnStartup(n9e)

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
		logger.Infof("balancealert cron: prepaid=%d excluded=%d skipped=%d sent=%d cooldown=%d failed=%d",
			stats.Prepaid, stats.Excluded, stats.SkippedIncome, stats.Sent, stats.Cooldown, stats.Failed)
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
		logger.Infof("balancealert dynamic cron: prepaid=%d excluded=%d evaluated=%d sent=%d cooldown=%d failed=%d",
			stats.Prepaid, stats.Excluded, stats.Evaluated, stats.Sent, stats.Cooldown, stats.Failed)
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

	// 排除名单每日 07:30（Asia/Shanghai）全量刷新。独立调度器显式绑定时区：
	// 不能用默认调度器的进程本地时区，否则容器跑 UTC 时会在北京时间 15:30 才刷。
	refreshCron := cron.New(cron.WithLocation(locShanghai))
	_, err = refreshCron.AddFunc("30 7 * * *", func() {
		if _, err := RefreshCreditExcludes(n9e, TriggerCron); err != nil {
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

// refreshOnStartup 只在从未成功刷新过时补刷一次。
//
// 已有成功记录时不做任何事：名单最长滞后一天是已知且接受的局限，重启不该为此
// 多打一次计费库聚合查询。
func refreshOnStartup(n9e *ctx.Context) {
	state, err := models.BalanceAlertExcludeStateGet(n9e)
	if err != nil {
		logger.Errorf("balancealert exclude state startup: %v", err)
	}
	if err == nil && state.Ready() {
		return
	}
	if _, err := RefreshCreditExcludes(n9e, TriggerStartup); err != nil {
		logger.Errorf("balancealert exclude refresh startup: %v (fail closed until next refresh)", err)
	}
}
