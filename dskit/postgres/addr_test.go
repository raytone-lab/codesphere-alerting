package postgres

import (
	"net/url"
	"strings"
	"testing"
)

func TestSplitHostDatabase(t *testing.T) {
	tests := []struct {
		addr     string
		wantHost string
		wantDB   string
	}{
		{addr: "127.0.0.1:5432", wantHost: "127.0.0.1:5432"},
		{addr: "127.0.0.1:5432/ruidong_admin", wantHost: "127.0.0.1:5432", wantDB: "ruidong_admin"},
		{addr: "127.0.0.1:5432/ruidong_admin/", wantHost: "127.0.0.1:5432", wantDB: "ruidong_admin"},
		{addr: "127.0.0.1:5432/ruidong_admin/postgres", wantHost: "127.0.0.1:5432", wantDB: "ruidong_admin"},
		{addr: "127.0.0.1:5432/ruidong_admin?sslmode=disable", wantHost: "127.0.0.1:5432", wantDB: "ruidong_admin"},
		{addr: "postgres://u:p@127.0.0.1:5432/ruidong_admin", wantHost: "127.0.0.1:5432", wantDB: "ruidong_admin"},
		{addr: "[::1]:5432/appdb", wantHost: "[::1]:5432", wantDB: "appdb"},
		{addr: "  127.0.0.1:5432/appdb  ", wantHost: "127.0.0.1:5432", wantDB: "appdb"},
		{addr: "", wantHost: "", wantDB: ""},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			host, db := SplitHostDatabase(tt.addr)
			if host != tt.wantHost || db != tt.wantDB {
				t.Fatalf("SplitHostDatabase(%q) = %q, %q; want %q, %q", tt.addr, host, db, tt.wantHost, tt.wantDB)
			}
		})
	}
}

func TestSslModeForAddr(t *testing.T) {
	tests := []struct {
		addr, host, want string
	}{
		{addr: "127.0.0.1:5432/ruidong_billing", host: "127.0.0.1:5432", want: "disable"},
		{addr: "localhost:5432/app", host: "localhost:5432", want: "disable"},
		{addr: "rds.example.com:5432/app", host: "rds.example.com:5432", want: "require"},
		{addr: "127.0.0.1:5432/app?sslmode=require", host: "127.0.0.1:5432", want: "require"},
	}
	for _, tt := range tests {
		got := sslModeForAddr(tt.addr, tt.host)
		if got != tt.want {
			t.Errorf("sslModeForAddr(%q,%q)=%q want %q", tt.addr, tt.host, got, tt.want)
		}
	}
}

// TestConnectTimeoutForAddr 锁定建连超时的取值来源：地址上显式声明优先，
// 否则用数据源的 pgsql.timeout。两者都没有时不能凭空编一个值（返回 0 表示不写进 DSN）。
func TestConnectTimeoutForAddr(t *testing.T) {
	tests := []struct {
		addr     string
		fallback int
		want     int
	}{
		{addr: "127.0.0.1:5432/ruidong_billing", fallback: 60, want: 60},
		{addr: "rds.example.com:5432/app?connect_timeout=15", fallback: 60, want: 15},
		{addr: "rds.example.com:5432/app?sslmode=require&connect_timeout=5", fallback: 60, want: 5},
		// 非法或非正数不能把连接变成「立刻失败」，退回数据源配置。
		{addr: "rds.example.com:5432/app?connect_timeout=0", fallback: 60, want: 60},
		{addr: "rds.example.com:5432/app?connect_timeout=-1", fallback: 60, want: 60},
		{addr: "rds.example.com:5432/app?connect_timeout=abc", fallback: 60, want: 60},
		{addr: "", fallback: 0, want: 0},
	}
	for _, tt := range tests {
		if got := connectTimeoutForAddr(tt.addr, tt.fallback); got != tt.want {
			t.Errorf("connectTimeoutForAddr(%q,%d)=%d want %d", tt.addr, tt.fallback, got, tt.want)
		}
	}
}

// TestBuildConnDSNCarriesConnectTimeout 是这条修复的核心断言。
//
// 没有 connect_timeout 时，被防火墙 DROP 的主机只能靠操作系统 SYN 重试
// （Linux 约 127s），远比查询 ctx 的 deadline 长，而且期间连错误都报不出来。
// 这里直接解析生成的 DSN，确认 pgx 会读到这个参数。
func TestBuildConnDSNCarriesConnectTimeout(t *testing.T) {
	shard := Shard{
		Addr:     "billing-db.example.com:5432/ruidong_billing",
		User:     "reader@corp",
		Password: "p@ss:word",
		Timeout:  60,
	}
	dsn := buildConnDSN(shard, "billing-db.example.com:5432", "ruidong_billing", connectTimeoutForAddr(shard.Addr, shard.Timeout))

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn %q: %v", dsn, err)
	}
	if got := parsed.Query().Get("connect_timeout"); got != "60" {
		t.Fatalf("connect_timeout = %q, want 60 (dsn=%s)", got, dsn)
	}
	// 顺手确认原有行为没被破坏：sslmode 仍按地址推断，账号密码仍被转义。
	if got := parsed.Query().Get("sslmode"); got != "disable" && got != "require" {
		t.Fatalf("sslmode = %q", got)
	}
	if parsed.User.Username() != shard.User {
		t.Fatalf("user = %q, want %q", parsed.User.Username(), shard.User)
	}
	if pw, _ := parsed.User.Password(); pw != shard.Password {
		t.Fatalf("password not round-tripped: %q", pw)
	}
}

// TestBuildConnDSNOmitsZeroTimeout 确认「数据源没配 timeout」时不写入该参数：
// 写 0 在 pgx 里等价于无限等待，反而不如交给驱动默认行为。
func TestBuildConnDSNOmitsZeroTimeout(t *testing.T) {
	dsn := buildConnDSN(Shard{Addr: "127.0.0.1:5432/app"}, "127.0.0.1:5432", "app", 0)
	if strings.Contains(dsn, "connect_timeout") {
		t.Fatalf("zero timeout must not be rendered into the DSN: %s", dsn)
	}
}
