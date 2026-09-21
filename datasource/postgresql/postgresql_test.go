package postgresql

import (
	"testing"

	"github.com/ccfos/nightingale/v6/dskit/pool"
	"github.com/ccfos/nightingale/v6/dskit/postgres"
	"gorm.io/gorm"
)

func TestInitDecodesConfiguredDatabase(t *testing.T) {
	p := new(PostgreSQL)
	datasource, err := p.Init(map[string]interface{}{
		"pgsql.shards": []map[string]interface{}{
			{"pgsql.db": "customdb"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := datasource.(*PostgreSQL)
	if len(got.Shards) != 1 {
		t.Fatalf("expected one shard, got %d", len(got.Shards))
	}
	if got.Shards[0].DB != "customdb" {
		t.Fatalf("expected database %q, got %q", "customdb", got.Shards[0].DB)
	}
}

func TestInitClientUsesConfiguredDatabase(t *testing.T) {
	shard := &postgres.PostgreSQL{
		Shard: postgres.Shard{
			Addr:     "127.0.0.1:1",
			DB:       "customdb",
			User:     "configured-db-user",
			Password: "configured-db-password",
		},
	}
	cacheKey := "127.0.0.1:1:configured-db-password:configured-db-user:customdb"
	pool.PoolClient.Store(cacheKey, &gorm.DB{})
	t.Cleanup(func() { pool.PoolClient.Delete(cacheKey) })

	if err := (&PostgreSQL{Shards: []*postgres.PostgreSQL{shard}}).InitClient(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitClientFallsBackToPostgres(t *testing.T) {
	shard := &postgres.PostgreSQL{
		Shard: postgres.Shard{
			Addr:     "127.0.0.1:1",
			User:     "fallback-db-user",
			Password: "fallback-db-password",
		},
	}
	cacheKey := "127.0.0.1:1:fallback-db-password:fallback-db-user:postgres"
	pool.PoolClient.Store(cacheKey, &gorm.DB{})
	t.Cleanup(func() { pool.PoolClient.Delete(cacheKey) })

	if err := (&PostgreSQL{Shards: []*postgres.PostgreSQL{shard}}).InitClient(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitClientUsesDatabaseFromAddr(t *testing.T) {
	shard := &postgres.PostgreSQL{
		Shard: postgres.Shard{
			Addr:     "127.0.0.1:1/ruidong_admin",
			User:     "addr-db-user",
			Password: "addr-db-password",
		},
	}
	cacheKey := "127.0.0.1:1:addr-db-password:addr-db-user:ruidong_admin"
	pool.PoolClient.Store(cacheKey, &gorm.DB{})
	t.Cleanup(func() { pool.PoolClient.Delete(cacheKey) })

	if err := (&PostgreSQL{Shards: []*postgres.PostgreSQL{shard}}).InitClient(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseDBName(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		want    string
		wantErr bool
	}{
		{name: "three-part", sql: "SELECT 1 FROM mydb.public.orders", want: "mydb"},
		{name: "quoted db", sql: `select 1 from "MyDB".public.orders`, want: "MyDB"},
		{name: "quoted after format", sql: `select 1 from "mydb"."public"."orders"`, want: "mydb"},
		{name: "schema.table only", sql: "SELECT 1 FROM public.orders", wantErr: true},
		{name: "bare table", sql: "SELECT 1 FROM orders", wantErr: true},
		{name: "empty", sql: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDBName(tt.sql)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got db %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveDatabase(t *testing.T) {
	tests := []struct {
		name    string
		shardDB string
		addr    string
		qp      *QueryParam
		want    string
	}{
		{
			name:    "query database wins",
			shardDB: "configured",
			qp:      &QueryParam{Database: "from-query", SQL: "SELECT 1 FROM other.public.t"},
			want:    "from-query",
		},
		{
			name:    "sql three-part when query database empty",
			shardDB: "configured",
			qp:      &QueryParam{SQL: "SELECT 1 FROM parsed.public.t"},
			want:    "parsed",
		},
		{
			name:    "datasource db when sql is schema.table",
			shardDB: "configured",
			qp:      &QueryParam{SQL: "SELECT 1 FROM public.orders"},
			want:    "configured",
		},
		{
			name:    "datasource db when sql is bare table",
			shardDB: "configured",
			qp:      &QueryParam{SQL: "SELECT count(*) FROM orders"},
			want:    "configured",
		},
		{
			name: "database from addr host:port/db",
			addr: "127.0.0.1:5432/ruidong_admin",
			qp:   &QueryParam{SQL: "select count(*) as alert_count from public.alerts"},
			want: "ruidong_admin",
		},
		{
			name:    "default postgres when nothing set",
			shardDB: "",
			qp:      &QueryParam{SQL: "SELECT 1 FROM public.orders"},
			want:    "postgres",
		},
		{
			name:    "nil query uses shard then default",
			shardDB: "",
			qp:      nil,
			want:    "postgres",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveDatabase(tt.shardDB, tt.addr, tt.qp)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
