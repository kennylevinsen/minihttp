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
	General     *SiteConfigGeneral
	Cache       *SiteConfigCache
	Compression *SiteConfigCompression
}

type SiteConfigGeneral struct {
	NoDefaultFile bool
	DefaultFile   string
	FancyFolder   string
}

type SiteConfigCache struct {
	NoCacheFromMem   bool
	NoCacheFromDisk  bool
	CacheTimes       map[string]Duration
	DefaultCacheTime Duration
	Blacklist        []string
}

type SiteConfigCompression struct {
	NoCompressFromMem  bool
	NoCompressFromDisk bool
	MinSize            int
	MinRatio           float64
	Blacklist          []string
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
		General: &SiteConfigGeneral{
			DefaultFile: "index.html",
			FancyFolder: "/f/",
		},
		Cache: &SiteConfigCache{
			CacheTimes:       DefaultCacheTimes,
			DefaultCacheTime: Duration{Duration: time.Hour},
		},
		Compression: &SiteConfigCompression{
			Blacklist: []string{
				".jpg",
				".zip",
				".gz",
				".tgz",
			},
			MinSize:  256,
			MinRatio: 1.1,
		},
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

	if conf.General == nil {
		conf.General = DefaultSiteConfig.General
	}
	if conf.Cache == nil {
		conf.Cache = DefaultSiteConfig.Cache
	}
	if conf.Compression == nil {
		conf.Compression = DefaultSiteConfig.Compression
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
