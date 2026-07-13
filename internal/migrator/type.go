package migrator

type MySQLConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
}

type PostgreSQLConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
	Schema   string
}

type Config struct {
	MySQL      *MySQLConfig
	PostgreSQL *PostgreSQLConfig
}
