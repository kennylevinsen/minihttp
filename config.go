package main

import (
	"io/ioutil"
	"time"
)
import "github.com/naoina/toml"

type Config struct {
	Root string

	DefaultHost string
	LogFile     string
	LogLines    int
	Development bool

	HTTP    ConfigHTTP
	HTTPS   ConfigHTTPS
	Command ConfigCommand
}

type ConfigHTTP struct {
	Address string
}

type ConfigHTTPS struct {
	Address string
	Cert    string
	Key     string
}

type ConfigCommand struct {
	Address string
}

type SiteConfig struct {
	DefaultFile             string
	FancyFolder             string
	CacheTimes              map[string]Duration
	DefaultCacheTime        Duration
	MinimumCompressionRatio float64
}

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalTOML(data []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(data))
	return err
}

var (
	threeMonths       = Duration{Duration: 90 * 24 * time.Hour}
	oneWeek           = Duration{Duration: 7 * 24 * time.Hour}
	DefaultCacheTimes = map[string]Duration{
		".woff":  threeMonths,
		".woff2": threeMonths,
		".ttf":   threeMonths,
		".eot":   threeMonths,
		".otf":   threeMonths,
		".jpg":   threeMonths,
		".png":   threeMonths,
		".css":   oneWeek,
	}
	DefaultSiteConfig = SiteConfig{
		DefaultFile:             "index.html",
		FancyFolder:             "f/",
		CacheTimes:              DefaultCacheTimes,
		DefaultCacheTime:        Duration{Duration: time.Hour},
		MinimumCompressionRatio: 1.1,
	}
	DefaultConfig = Config{
		Root: "/srv/web",
		HTTP: ConfigHTTP{
			Address: ":80",
		},
		Command: ConfigCommand{
			Address: ":65001",
		},
	}
)

func readSiteConf(p string) (*SiteConfig, error) {
	var (
		b    []byte
		err  error
		conf SiteConfig
	)

	if b, err = ioutil.ReadFile(p); err != nil {
		return &DefaultSiteConfig, err
	}

	if err := toml.Unmarshal(b, &conf); err != nil {
		return nil, err
	}

	return &conf, nil
}

func readServerConf(p string) (*Config, error) {
	var (
		b    []byte
		err  error
		conf Config
	)

	if b, err = ioutil.ReadFile(p); err != nil {
		return &DefaultConfig, err
	}

	if err := toml.Unmarshal(b, &conf); err != nil {
		return nil, err
	}

	return &conf, nil
}
