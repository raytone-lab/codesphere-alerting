package postgres

import "testing"

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
