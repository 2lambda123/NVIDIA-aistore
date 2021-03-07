// Package config provides types and functions to configure AIS CLI.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/jsp"
)

const (
	configDirName  = "ais"
	configFileName = "config.json"

	urlFmt           = "%s://%s:%d"
	defaultAISIP     = "127.0.0.1"
	defaultAISPort   = 8080
	defaultAuthNPort = 52001
	defaultDockerIP  = "172.50.0.2"
)

var (
	ConfigDirPath string
	defaultConfig Config
)

func init() {
	ConfigDirPath = cmn.AppConfigPath(configDirName)

	proto := "http"
	if value := os.Getenv(cmn.EnvVars.UseHTTPS); cos.IsParseBool(value) {
		proto = "https"
	}
	aisURL := fmt.Sprintf(urlFmt, proto, defaultAISIP, defaultAISPort)
	defaultConfig = Config{
		Cluster: ClusterConfig{
			URL:               aisURL,
			DefaultAISHost:    aisURL,
			DefaultDockerHost: fmt.Sprintf(urlFmt, proto, defaultDockerIP, defaultAISPort),
			SkipVerifyCrt:     cos.IsParseBool(os.Getenv(cmn.EnvVars.SkipVerifyCrt)),
		},
		Timeout: TimeoutConfig{
			TCPTimeoutStr:  "60s",
			TCPTimeout:     60 * time.Second,
			HTTPTimeoutStr: "0s",
			HTTPTimeout:    0,
		},
		Auth: AuthConfig{
			URL: fmt.Sprintf(urlFmt, proto, defaultAISIP, defaultAuthNPort),
		},
	}
}

type Config struct {
	Cluster ClusterConfig `json:"cluster"`
	Timeout TimeoutConfig `json:"timeout"`
	Auth    AuthConfig    `json:"auth"`
}

type ClusterConfig struct {
	URL               string `json:"url"`
	DefaultAISHost    string `json:"default_ais_host"`
	DefaultDockerHost string `json:"default_docker_host"`
	SkipVerifyCrt     bool   `json:"skip_verify_crt"`
}

type TimeoutConfig struct {
	TCPTimeoutStr  string        `json:"tcp_timeout"`
	TCPTimeout     time.Duration `json:"-"`
	HTTPTimeoutStr string        `json:"http_timeout"`
	HTTPTimeout    time.Duration `json:"-"`
}

type AuthConfig struct {
	URL string `json:"url"`
}

func (c *Config) validate() (err error) {
	if c.Timeout.TCPTimeout, err = time.ParseDuration(c.Timeout.TCPTimeoutStr); err != nil {
		return fmt.Errorf("invalid timeout.tcp_timeout format %q: %v", c.Timeout.TCPTimeoutStr, err)
	}
	if c.Timeout.HTTPTimeout, err = time.ParseDuration(c.Timeout.HTTPTimeoutStr); err != nil {
		return fmt.Errorf("invalid timeout.http_timeout format %q: %v", c.Timeout.HTTPTimeoutStr, err)
	}
	return nil
}

func Load() (*Config, error) {
	cfg := &Config{}
	if err := jsp.LoadAppConfig(ConfigDirPath, configFileName, &cfg); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to load config: %v", err)
		}

		// Use default config in case of error.
		cfg = &defaultConfig
		err = jsp.SaveAppConfig(ConfigDirPath, configFileName, cfg)
		if err != nil {
			err = fmt.Errorf("failed to generate config file: %v", err)
		}
		return cfg, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
