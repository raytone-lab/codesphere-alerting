package postgres

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/dskit/sqlbase"

	"gorm.io/driver/postgres"
)

// TestNewDBHonorsConnectTimeout 是这条修复的端到端证明。
//
// 起一个「只完成 TCP 握手、永远不说话」的监听端，模拟数据库卡在启动握手的场景
// （防火墙 DROP、实例假死）。如果 DSN 里没有 connect_timeout，pgx 会一直等下去；
// 有了它，pgx 的注释写得很清楚——「ConnectTimeout restricts the whole connection
// process」，连拨号带握手一起算。
//
// 这条用例不依赖外网，只用本地回环，所以能进常规单测；耗时被 connect_timeout=1
// 钉在 1 秒量级。
func TestNewDBHonorsConnectTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// 接受连接但永不握手，并且把连接留住（立刻关闭会变成 EOF，测不出超时）。
	held := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held <- c
		}
	}()
	defer func() {
		close(held)
		for c := range held {
			_ = c.Close()
		}
	}()

	addr := ln.Addr().String()
	shard := Shard{Addr: addr, User: "u", Password: "p", Timeout: 1}
	dsn := buildConnDSN(shard, addr, "app", connectTimeoutForAddr(shard.Addr, shard.Timeout))

	start := time.Now()
	db, err := sqlbase.NewDB(context.Background(), postgres.Open(dsn), 1, 1, time.Minute)
	elapsed := time.Since(start)

	if err == nil {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		t.Fatalf("a silent listener must not be reported as a usable connection (dsn=%s)", dsn)
	}
	// 上限给 5s：Windows 的定时器精度与握手重试都在毫秒到秒级，1s 的超时不应
	// 变成十几秒。真正要防的是「无限等待」（Linux 上 SYN 重试约 127s）。
	if elapsed > 5*time.Second {
		t.Fatalf("connect must be bounded by connect_timeout=1s, took %s", elapsed)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("returned in %s, too fast to have attempted the handshake", elapsed)
	}
}
