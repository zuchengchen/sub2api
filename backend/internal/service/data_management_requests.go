package service

// These legacy request types only preserve JSON binding behavior for retired
// data-management routes. Keep their names, field types and tags: invalid input
// must still return the same validation errors before the deprecation response.
// They are not used to configure a live agent or the independent BackupService.
type DataManagementPostgresConfig struct {
	Host               string `json:"host"`
	Port               int32  `json:"port"`
	User               string `json:"user"`
	Password           string `json:"password,omitempty"`
	PasswordConfigured bool   `json:"password_configured"`
	Database           string `json:"database"`
	SSLMode            string `json:"ssl_mode"`
	ContainerName      string `json:"container_name"`
}

type DataManagementRedisConfig struct {
	Addr               string `json:"addr"`
	Username           string `json:"username"`
	Password           string `json:"password,omitempty"`
	PasswordConfigured bool   `json:"password_configured"`
	DB                 int32  `json:"db"`
	ContainerName      string `json:"container_name"`
}

type DataManagementS3Config struct {
	Enabled                   bool   `json:"enabled"`
	Endpoint                  string `json:"endpoint"`
	Region                    string `json:"region"`
	Bucket                    string `json:"bucket"`
	AccessKeyID               string `json:"access_key_id"`
	SecretAccessKey           string `json:"secret_access_key,omitempty"`
	SecretAccessKeyConfigured bool   `json:"secret_access_key_configured"`
	Prefix                    string `json:"prefix"`
	ForcePathStyle            bool   `json:"force_path_style"`
	UseSSL                    bool   `json:"use_ssl"`
}

type DataManagementConfig struct {
	SourceMode        string                       `json:"source_mode"`
	BackupRoot        string                       `json:"backup_root"`
	SQLitePath        string                       `json:"sqlite_path,omitempty"`
	RetentionDays     int32                        `json:"retention_days"`
	KeepLast          int32                        `json:"keep_last"`
	ActivePostgresID  string                       `json:"active_postgres_profile_id"`
	ActiveRedisID     string                       `json:"active_redis_profile_id"`
	Postgres          DataManagementPostgresConfig `json:"postgres"`
	Redis             DataManagementRedisConfig    `json:"redis"`
	S3                DataManagementS3Config       `json:"s3"`
	ActiveS3ProfileID string                       `json:"active_s3_profile_id"`
}

type DataManagementSourceConfig struct {
	Host          string `json:"host"`
	Port          int32  `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password,omitempty"`
	Database      string `json:"database"`
	SSLMode       string `json:"ssl_mode"`
	Addr          string `json:"addr"`
	Username      string `json:"username"`
	DB            int32  `json:"db"`
	ContainerName string `json:"container_name"`
}
