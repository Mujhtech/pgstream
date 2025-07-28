package config

type DatabaseDriver string
type CacheProvider string
type TelemetryProvider string

const (
	DatabaseDriverPostgres DatabaseDriver = "postgres"
	DatabaseDriverSqlite3  DatabaseDriver = "sqlite3"

	DefaultConfigFilePath string = ".env"

	CacheProviderRedis    CacheProvider = "redis"
	CacheProviderInMemory CacheProvider = "inmemory"

	TelemetryProviderSentry TelemetryProvider = "sentry"
	TelemetryProviderOtel   TelemetryProvider = "otel"
	TelemetryProviderNone   TelemetryProvider = "none"
)

type Config struct {
	EncryptionKey string `json:"encryption_key" envconfig:"ENCRYPTION_KEY"`
	Server        Server `json:"server"`
	Cors          Cors   `json:"cors"`
	Cache         Cache  `json:"cache"`
}

type Cache struct {
	Provider CacheProvider `json:"provider" envconfig:"CACHE_PROVIDER"`
}

// Cors defines CORS configuration
type Cors struct {
	AllowedOrigins   []string `json:"allowed_origins" envconfig:"CORS_ALLOWED_ORIGINS"`
	AllowedMethods   []string `json:"allowed_methods" envconfig:"CORS_ALLOWED_METHODS"`
	AllowedHeaders   []string `json:"allowed_headers" envconfig:"CORS_ALLOWED_HEADERS"`
	ExposedHeaders   []string `json:"exposed_headers" envconfig:"CORS_EXPOSED_HEADERS"`
	AllowCredentials bool     `json:"allow_credentials" envconfig:"CORS_ALLOW_CREDENTIALS"`
	MaxAge           int      `json:"max_age" envconfig:"CORS_MAX_AGE"`
}

// Server defines server configuration
type Server struct {
	Port        uint32 `json:"port" envconfig:"PORT"`
	SSL         bool   `json:"ssl" envconfig:"SSL"`
	SSLCertFile string `json:"ssl_cert_file" envconfig:"SSL_CERT_FILE"`
	SSLKeyFile  string `json:"ssl_key_file" envconfig:"SSL_KEY_FILE"`
	Timeout     uint32 `json:"timeout" envconfig:"TIMEOUT"`
}

// Redis defines redis configuration
type Redis struct {
	Host               string `json:"host" envconfig:"REDIS_HOST"`
	Port               int    `json:"port" envconfig:"REDIS_PORT"`
	Username           string `json:"username" envconfig:"REDIS_USERNAME"`
	Password           string `json:"password" envconfig:"REDIS_PASSWORD"`
	MaxRetries         int    `json:"max_retries" envconfig:"REDIS_MAX_RETRIES"`
	MinIdleConnections int    `json:"min_idle_connections" envconfig:"REDIS_MIN_IDLE_CONNECTIONS"`
	DB                 int    `json:"db" envconfig:"REDIS_DB"`
}
