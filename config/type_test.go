package config

import "testing"

func TestBuildDSNUsesDriverSpecificNetworkSyntax(t *testing.T) {
	tests := []struct {
		name string
		db   Database
		want string
	}{
		{
			name: "mysql localhost",
			db: Database{
				Driver: DatabaseDriverMySQL, Host: "localhost", Port: 3306,
				User: "root", Password: "secret", Database: "app", Options: "parseTime=true",
			},
			want: "root:secret@tcp(localhost:3306)/app?parseTime=true",
		},
		{
			name: "postgres remote",
			db: Database{
				Driver: DatabaseDriverPostgres, Host: "db.example.com", Port: 5432,
				User: "app", Password: "p@ss word", Database: "production", Options: "sslmode=require",
			},
			want: "postgres://app:p%40ss%20word@db.example.com:5432/production?sslmode=require",
		},
		{
			name: "postgres ipv6",
			db: Database{
				Driver: DatabaseDriverPostgres, Host: "2001:db8::1", Port: 5432,
				Database: "app",
			},
			want: "postgres://[2001:db8::1]:5432/app",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.db.BuildDsn(); got != test.want {
				t.Fatalf("unexpected DSN\nwant: %s\n got: %s", test.want, got)
			}
		})
	}
}

func TestConfigRejectsUnsafeMetadataDatabaseSettings(t *testing.T) {
	tests := []Config{
		{Database: Database{Driver: DatabaseDriverSqlite3}},
		{Database: Database{Driver: DatabaseDriverPostgres, Host: "localhost", Port: 0}},
		{Database: Database{Driver: DatabaseDriverMySQL, Host: "localhost", Port: 3306}},
	}
	for _, config := range tests {
		if err := config.validate(); err == nil {
			t.Fatalf("expected invalid metadata configuration to fail: %#v", config.Database)
		}
	}
}

func TestSQLiteDSNOmitsEmptyQueryDelimiter(t *testing.T) {
	database := Database{Driver: DatabaseDriverSqlite3, Path: "metadata.db"}
	if got := database.BuildDsn(); got != "metadata.db" {
		t.Fatalf("unexpected SQLite DSN %q", got)
	}
}
