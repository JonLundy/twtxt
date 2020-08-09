package internal

import (
	"encoding/json"
	"io/ioutil"
	"math/rand"
	"os"
	"time"
)

// Config contains the server configuration parameters
type Config struct {
	Data            string        `json:"data"`
	Name            string        `json:"name"`
	Store           string        `json:"store"`
	Theme           string        `json:"theme"`
	BaseURL         string        `json:"base_url"`
	AdminUser       string        `json:"admin_user"`
	FeedSources     []string      `json:"feed_sources"`
	Register        bool          `json:"register"`
	RegisterMessage string        `json:"register_message"`
	CookieSecret    string        `json:"cookie_secret"`
	TwtPrompts      []string      `json:"twt_prompts"`
	TwtsPerPage     int           `json:"twts_per_page"`
	MaxUploadSize   int64         `json:"max_upload_size"`
	MaxTwtLength    int           `json:"max_twt_length"`
	OpenProfiles    bool          `json:"open_profiles"`
	SessionExpiry   time.Duration `json:"session_expiry"`

	MagicLinkSecret string `json:"magiclink_secret"`

	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	SMTPUser string `json:"smtp_user"`
	SMTPPass string `json:"smtp_pass"`
	SMTPFrom string `json:"smtp_from"`

	MaxFetchLimit int64 `json:"max_fetch_limit"`

	APISessionTime time.Duration `json:"api_session_time"`
	APISigningKey  []byte        `json:"api_signing_key"`
}

// RandomTwtPrompt returns a random  Twt Prompt for display by the UI
func (c *Config) RandomTwtPrompt() string {
	n := rand.Int() % len(c.TwtPrompts)
	return c.TwtPrompts[n]
}

// Load loads a configuration from the given path
func Load(path string) (*Config, error) {
	var cfg Config

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Save saves the configuration to the provided path
func (c *Config) Save(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}

	data, err := json.Marshal(c)
	if err != nil {
		return err
	}

	if _, err = f.Write(data); err != nil {
		return err
	}

	if err = f.Sync(); err != nil {
		return err
	}

	return f.Close()
}
