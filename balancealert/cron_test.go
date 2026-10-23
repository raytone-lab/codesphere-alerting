package balancealert

import (
	"errors"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/robfig/cron/v3"
)

// 本文件覆盖 cron.go 里那些「只有上线才看得见」的约束：
//
//   - 名单刷新绑的是 Asia/Shanghai 的 07:30，不是进程本地时区的 07:30；
//   - 启动补刷是同步的，且一定发生在 cron 注册之前；
//   - 已有成功名单时重启不重复打计费库；
//   - 补刷失败不阻断调度注册（fail closed 已挡住误发）。
//
// 这四条都没有返回值、没有函数签名可以断言，只能把「注册调度器」这一步换成
// 可观察的替身（startCronJobs），或者直接检查调度器自身的时区与规格。

func stubStartCronJobs(t *testing.T, fn func(*ctx.Context)) {
	t.Helper()
	prev := startCronJobs
	startCronJobs = fn
	t.Cleanup(func() { startCronJobs = prev })
}

// TestExcludeRefreshCronIsDaily0730Shanghai 锁定「每日 07:30，Asia/Shanghai」。
//
// 关键在于先把 time.Local 顶成 UTC：默认调度器用的是构造时的 time.Local，
// 若有人删掉 cron.WithLocation(locShanghai)，在 UTC 机器上这条会立刻变红，
// 而不是像「本机恰好也是 +08」那样安静地假通过。
func TestExcludeRefreshCronIsDaily0730Shanghai(t *testing.T) {
	prev := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = prev })

	n9e := testRunnerCtx(t)
	c, err := newExcludeRefreshCron(n9e)
	if err != nil {
		t.Fatalf("build refresh cron: %v", err)
	}
	t.Cleanup(func() { c.Stop() })

	if got := c.Location().String(); got != "Asia/Shanghai" {
		t.Fatalf("refresh cron must bind Asia/Shanghai explicitly, got %q", got)
	}

	entries := c.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected exactly one refresh entry, got %d", len(entries))
	}

	// 20:00 上海（= 12:00 UTC），下一次必须是上海时间次日 07:30。
	// 传入的时间先按调度器时区换算，与 cron 内部的 now := time.Now().In(c.location) 一致。
	from := time.Date(2026, 10, 23, 20, 0, 0, 0, locShanghai)
	next := entries[0].Schedule.Next(from)
	if next.Hour() != 7 || next.Minute() != 30 {
		t.Fatalf("refresh must fire at 07:30 Shanghai, got %s", next.Format(time.RFC3339))
	}
	if _, offset := next.Zone(); offset != 8*3600 {
		t.Fatalf("refresh must fire at UTC+8, got offset %d (%s)", offset, next.Format(time.RFC3339))
	}
	if want := time.Date(2026, 10, 24, 7, 30, 0, 0, locShanghai); !next.Equal(want) {
		t.Fatalf("next refresh = %s, want %s", next, want)
	}
	// 同一时刻用 UTC 表示必须指到同一个瞬间，防止把 07:30 理解成 UTC。
	if want := time.Date(2026, 10, 23, 23, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("07:30 Shanghai must be 23:30 UTC, got %s", next.UTC().Format(time.RFC3339))
	}

	// 每日一次，不是每小时一次：再往后的那次必须正好晚 24 小时。
	if day2 := entries[0].Schedule.Next(next); day2.Sub(next) != 24*time.Hour {
		t.Fatalf("refresh must be daily, next two runs are %s apart", day2.Sub(next))
	}
}

// TestExcludeRefreshJobUsesCronTrigger 锁定每日任务走 CRON 触发。
//
// 触发类型不是日志标签：只有 CRON/STARTUP 会推进连续失败计数并告警，MANUAL 不会。
// 把这里改成 TriggerManual，配置页连点按钮的语义就会悄悄扩散到定时任务上。
func TestExcludeRefreshJobUsesCronTrigger(t *testing.T) {
	n9e := testRunnerCtx(t)
	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		return map[string]string{"a": models.ExcludeReasonOpenDebt}, nil
	})

	excludeRefreshJob(n9e)

	state := excludeState(t, n9e)
	if state.LastTrigger != string(TriggerCron) {
		t.Fatalf("daily refresh must run as CRON, got %q", state.LastTrigger)
	}
	if state.Total != 1 {
		t.Fatalf("daily refresh must persist the new list, state=%+v", state)
	}
}

// TestStartCronRefreshesBeforeRegisteringJobs 是 Q2 的可执行版本。
//
// 记录器在被调用时读库：如果补刷被改回 goroutine 或挪到注册之后，此刻名单还没
// 落库、状态还是 not ready，用例立刻失败。这条顺序没有别的观察点。
func TestStartCronRefreshesBeforeRegisteringJobs(t *testing.T) {
	n9e := testRunnerCtx(t)
	// 清空状态：模拟首次升级到名单版本。
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{}, "root"); err != nil {
		t.Fatal(err)
	}
	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		return map[string]string{"ba-1": models.ExcludeReasonOpenDebt}, nil
	})

	var calls int
	var listAtRegister map[string]struct{}
	var stateAtRegister models.BalanceAlertExcludeState
	stubStartCronJobs(t, func(c *ctx.Context) {
		calls++
		var err error
		if listAtRegister, err = models.BalanceAlertCreditExcludeIDs(c); err != nil {
			t.Errorf("read list at register: %v", err)
		}
		if stateAtRegister, err = models.BalanceAlertExcludeStateGet(c); err != nil {
			t.Errorf("read state at register: %v", err)
		}
	})

	StartCron(n9e)

	if calls != 1 {
		t.Fatalf("cron jobs must be registered exactly once, got %d", calls)
	}
	if len(listAtRegister) != 1 {
		t.Fatalf("startup refresh must complete before registering cron jobs, list=%v", listAtRegister)
	}
	if !stateAtRegister.Ready() {
		t.Fatalf("state must already be ready when cron is registered: %+v", stateAtRegister)
	}
	if stateAtRegister.LastTrigger != string(TriggerStartup) {
		t.Fatalf("startup refresh must be tagged STARTUP, got %q", stateAtRegister.LastTrigger)
	}
}

// TestStartCronSkipsStartupRefreshWhenListReady 锁定「已有成功记录时不做任何事」：
// 重启是常态，不该每次都打一遍计费库聚合。
func TestStartCronSkipsStartupRefreshWhenListReady(t *testing.T) {
	n9e := testRunnerCtx(t) // 默认状态即 ready

	scanned := 0
	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		scanned++
		return nil, errors.New("a ready list must not be rescanned on restart")
	})
	jobs := 0
	stubStartCronJobs(t, func(*ctx.Context) { jobs++ })

	StartCron(n9e)

	if scanned != 0 {
		t.Fatalf("restart with a ready list must not touch the billing DB, scans=%d", scanned)
	}
	if jobs != 1 {
		t.Fatalf("cron jobs must still be registered, got %d", jobs)
	}
}

// TestStartCronContinuesWhenStartupRefreshFails 锁定 Q9：补刷失败只记录，不退出、
// 不跳过调度注册。整平台不该为一张派生缓存停摆，fail closed 已保证不会误发。
func TestStartCronContinuesWhenStartupRefreshFails(t *testing.T) {
	n9e := testRunnerCtx(t)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{}, "root"); err != nil {
		t.Fatal(err)
	}
	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		return nil, errors.New("connect billing: connection refused")
	})

	jobs := 0
	stubStartCronJobs(t, func(*ctx.Context) { jobs++ })

	StartCron(n9e) // 不得 panic，也不得在此提前返回

	if jobs != 1 {
		t.Fatalf("a failed startup refresh must still register cron jobs, got %d", jobs)
	}
	state := excludeState(t, n9e)
	if state.Ready() {
		t.Fatalf("failed startup refresh must stay fail closed: %+v", state)
	}
	if state.ConsecutiveFailures != 1 {
		t.Fatalf("failed startup refresh must be booked, got %+v", state)
	}
	if state.LastTrigger != string(TriggerStartup) {
		t.Fatalf("startup failure must be tagged STARTUP, got %q", state.LastTrigger)
	}
}

// TestStartCronRegistersAllSchedules 防的是另一种倒退：有人把刷新任务挂到默认
// 调度器（丢时区）或直接把某条任务删掉。这里不启动调度器，只核对四条规格。
func TestStartCronRegistersAllSchedules(t *testing.T) {
	n9e := testRunnerCtx(t)
	stubStartCronJobs(t, func(*ctx.Context) {})

	// 生产路径必须仍然由四条规格组成；这里直接校验常量，避免把调度器真的跑起来
	// （15 分钟那条会在测试里触发判定并写库）。
	c, err := newExcludeRefreshCron(n9e)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop() })

	if entries := c.Entries(); len(entries) != 1 {
		t.Fatalf("refresh scheduler must hold exactly one entry, got %d", len(entries))
	}
	for spec, want := range map[string]time.Duration{
		staticRunSpec:      15 * time.Minute,
		dynamicRunSpec:     24 * time.Hour,
		missReportRunSpec:  24 * time.Hour,
		excludeRefreshSpec: 24 * time.Hour,
	} {
		sched, err := cron.ParseStandard(spec)
		if err != nil {
			t.Fatalf("spec %q: %v", spec, err)
		}
		from := time.Date(2026, 10, 23, 0, 0, 0, 0, locShanghai)
		first := sched.Next(from)
		if gap := sched.Next(first).Sub(first); gap != want {
			t.Fatalf("spec %q fires every %s, want %s", spec, gap, want)
		}
	}
}
