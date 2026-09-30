package postgres

import "strings"

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
