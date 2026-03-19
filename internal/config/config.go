package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Accounts    []string    `yaml:"accounts"`
	Regions     []string    `yaml:"regions"`
	AssumeRole  string      `yaml:"assumeRole"`
	AWSProfile  string      `yaml:"awsProfile"`
	Concurrency Concurrency `yaml:"concurrency"`
}

type Concurrency struct {
	MaxWorkers       int    `yaml:"maxWorkers"`
	RetryMaxAttempts int    `yaml:"retryMaxAttempts"`
	RetryMaxBackoff  string `yaml:"retryMaxBackoff"`
}

func (c *Concurrency) BackoffDuration() (time.Duration, error) {
	if c.RetryMaxBackoff == "" {
		return 30 * time.Second, nil
	}
	return time.ParseDuration(c.RetryMaxBackoff)
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{
		// Defaults
		Concurrency: Concurrency{
			MaxWorkers:       10,
			RetryMaxAttempts: 5,
			RetryMaxBackoff:  "30s",
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	if len(c.Accounts) == 0 {
		return fmt.Errorf("no accounts specified in config")
	}
	if len(c.Regions) == 0 {
		return fmt.Errorf("no regions specified in config")
	}
	if c.AssumeRole == "" {
		return fmt.Errorf("assumeRole must be specified in config")
	}
	if c.Concurrency.MaxWorkers < 1 {
		return fmt.Errorf("concurrency.maxWorkers must be >= 1")
	}
	if c.Concurrency.RetryMaxAttempts < 1 {
		return fmt.Errorf("concurrency.retryMaxAttempts must be >= 1")
	}
	if _, err := c.Concurrency.BackoffDuration(); err != nil {
		return fmt.Errorf("invalid concurrency.retryMaxBackoff: %w", err)
	}
	return nil
}

// WorkItems returns every (account, region) tuple as a flat list.
func (c *Config) WorkItems() []WorkItem {
	items := make([]WorkItem, 0, len(c.Accounts)*len(c.Regions))
	for _, acct := range c.Accounts {
		for _, region := range c.Regions {
			items = append(items, WorkItem{
				AccountID: acct,
				Region:    region,
			})
		}
	}
	return items
}

type WorkItem struct {
	AccountID string
	Region    string
}
