package config

import (
	"fmt"

	"github.com/kelseyhightower/envconfig"
)

var DefaultConfig = &Config{
	Database: Database{
		Driver: DatabaseDriverSqlite3,
		Path:   "pgstream.db",
	},
}

func LoadConfig() (*Config, error) {
	config := *DefaultConfig

	// Override config from environment variables
	err := envconfig.Process("", &config)
	if err != nil {
		return nil, err
	}

	if err = config.validate(); err != nil {
		return nil, err
	}

	return &config, nil
}

func (c *Config) validate() error {
	switch c.Database.Driver {
	case DatabaseDriverSqlite3:
		if c.Database.Path == "" {
			return fmt.Errorf("SQLite metadata path cannot be empty")
		}
	case DatabaseDriverPostgres:
		if c.Database.Host == "" {
			return fmt.Errorf("metadata database host cannot be empty")
		}
		if c.Database.Port < 1 || c.Database.Port > 65535 {
			return fmt.Errorf("metadata database port must be between 1 and 65535")
		}
	default:
		return fmt.Errorf("unsupported metadata database driver %q", c.Database.Driver)
	}

	dbDsn := c.Database.BuildDsn()
	if dbDsn == "" {
		return fmt.Errorf("database dsn is empty")
	}

	return nil
}
