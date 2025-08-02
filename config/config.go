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
	config := DefaultConfig

	// Override config from environment variables
	err := envconfig.Process("", config)
	if err != nil {
		return nil, err
	}

	if err = config.validate(); err != nil {
		return nil, err
	}

	return config, nil
}

func (c *Config) validate() error {
	// Validate database configuration
	if c.Database.Host == "" && c.Database.Driver != DatabaseDriverSqlite3 {
		return fmt.Errorf("database host cannot be empty")
	}
	if c.Database.Port == 0 && c.Database.Driver != DatabaseDriverSqlite3 {
		return fmt.Errorf("database port cannot be zero")
	}

	dbDsn := c.Database.BuildDsn()
	if dbDsn == "" {
		return fmt.Errorf("database dsn is empty")
	}

	return nil
}
