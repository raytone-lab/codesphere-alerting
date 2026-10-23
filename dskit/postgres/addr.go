package postgres

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// SplitHostDatabase splits a PostgreSQL address into host:port and database.
// The datasource form has no separate database field, so users commonly put
// the database in the address as host:port/dbname.
func SplitHostDatabase(addr string) (host, database string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", ""
	}
	addr = strings.TrimPrefix(addr, "postgres://")
	addr = strings.TrimPrefix(addr, "postgresql://")
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		addr = addr[at+1:]
	}
	if q := strings.Index(addr, "?"); q >= 0 {
		addr = addr[:q]
	}

	host = addr
	rest := ""
	if strings.HasPrefix(addr, "[") {
		end := strings.Index(addr, "]")
		if end >= 0 {
			after := addr[end+1:]
			if slash := strings.Index(after, "/"); slash >= 0 {
				host = addr[:end+1+slash]
				rest = after[slash+1:]
			}
			return host, firstPathSegment(rest)
		}
	}

	if slash := strings.Index(addr, "/"); slash >= 0 {
		host = addr[:slash]
		rest = addr[slash+1:]
	}
	return host, firstPathSegment(rest)
}

func firstPathSegment(rest string) string {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return ""
	}
	if slash := strings.Index(rest, "/"); slash >= 0 {
		return rest[:slash]
	}
	return rest
}

func sslModeForAddr(addr, host string) string {
	raw := strings.TrimSpace(addr)
	if q := strings.Index(raw, "?"); q >= 0 {
		for _, part := range strings.Split(raw[q+1:], "&") {
			if strings.HasPrefix(part, "sslmode=") {
				mode := strings.TrimSpace(strings.TrimPrefix(part, "sslmode="))
				if mode != "" {
					return mode
				}
			}
		}
	}
	h := host
	if strings.HasPrefix(h, "[") {
		end := strings.Index(h, "]")
		if end > 0 {
			h = h[1:end]
		}
	} else if i := strings.LastIndex(h, ":"); i > 0 {
		h = h[:i]
	}
	switch h {
	case "127.0.0.1", "localhost", "::1":
		return "disable"
	default:
		return "require"
	}
}

// connectTimeoutForAddr 返回建连超时的秒数。
//
// 地址上显式写的 ?connect_timeout= 优先（与 sslModeForAddr 的约定一致），否则用
// 数据源自带的 pgsql.timeout（NewConn 已把 0 兜底成 60）。
//
// 这个值只作用于「拨号 + 启动握手」这一步，和查询超时是两码事：pgx 与 lib/pq 都在
// ctx 之外发起拨号，所以 ctx 的 deadline 管不到建连。少了它，对方主机被防火墙
// DROP 时只能等操作系统的 SYN 重试（Linux 约 127 秒），比整条刷新链路的查询预算
// 还长，而且这段时间连错误都报不出来。
func connectTimeoutForAddr(addr string, fallback int) int {
	raw := strings.TrimSpace(addr)
	if q := strings.Index(raw, "?"); q >= 0 {
		for _, part := range strings.Split(raw[q+1:], "&") {
			value, ok := strings.CutPrefix(part, "connect_timeout=")
			if !ok {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
				return n
			}
		}
	}
	return fallback
}

// buildConnDSN 组装 pgsql 连接串。connect_timeout 必须显式下发，见上。
func buildConnDSN(shard Shard, host, database string, connectTimeout int) string {
	dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s&TimeZone=Asia/Shanghai",
		url.QueryEscape(shard.User), url.QueryEscape(shard.Password), host, database, sslModeForAddr(shard.Addr, host))
	if connectTimeout > 0 {
		dsn += fmt.Sprintf("&connect_timeout=%d", connectTimeout)
	}
	return dsn
}
