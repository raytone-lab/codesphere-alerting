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

func firstPathSegment(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	if i := strings.Index(path, "/"); i >= 0 {
		path = path[:i]
	}
	return path
}
