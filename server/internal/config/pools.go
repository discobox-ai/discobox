package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
)

type PoolProxySettings struct {
	Mode    string `yaml:"mode" env:"DISCOBOX_POOLS_PROXY_MODE" envEmpty:"set" enum:"direct,explicit,inherit" example:"explicit" doc:"Pool outbound mode. Defaults to direct, independently of the server environment."`
	HTTP    string `yaml:"http" env:"DISCOBOX_POOLS_PROXY_HTTP" envEmpty:"set" example:"http://proxy.example:3128" doc:"HTTP origin forward proxy URL; empty means direct in explicit mode."`
	HTTPS   string `yaml:"https" env:"DISCOBOX_POOLS_PROXY_HTTPS" envEmpty:"set" example:"http://proxy.example:3128" doc:"HTTPS origin HTTP CONNECT proxy URL; empty means direct in explicit mode."`
	NoProxy string `yaml:"noProxy" env:"DISCOBOX_POOLS_PROXY_NO_PROXY" envEmpty:"set" example:".example.internal" doc:"Upstream bypass list. Does not bypass sandbox egress policy."`
	CAFile  string `yaml:"caFile" env:"DISCOBOX_POOLS_PROXY_CA_FILE" envEmpty:"set" example:"/etc/company/roots.pem" doc:"Server-local PEM CA bundle copied into pool trust stores before bootstrap."`
}

type PoolOverrideSettings struct {
	Proxy PoolProxyOverride `yaml:"proxy" doc:"Whole-object outbound override for this immutable pool ID."`
}

// PoolProxyOverride has no environment tags: global variables change only defaults.
type PoolProxyOverride struct {
	Mode    string `yaml:"mode" enum:"direct,explicit,inherit" example:"explicit" doc:"Outbound mode: direct, explicit, or inherit."`
	HTTP    string `yaml:"http" doc:"HTTP forward proxy URL."`
	HTTPS   string `yaml:"https" doc:"HTTPS origin HTTP CONNECT proxy URL."`
	NoProxy string `yaml:"noProxy" doc:"Upstream bypass list."`
	CAFile  string `yaml:"caFile" doc:"Server-local additional CA bundle."`
}

type PoolSettings struct {
	Proxy     PoolProxySettings               `yaml:"proxy" doc:"Default outbound routing and trust for pools."`
	Overrides map[string]PoolOverrideSettings `yaml:"overrides" example:"-" doc:"Whole-object outbound overrides keyed by immutable pool ID; unmatched IDs create nothing."`
}

func resolvePoolProxy(mode, http, https, noProxy, caFile string) (pooloutbound.Resolved, error) {
	i := pooloutbound.Intent{Mode: mode, HTTP: http, HTTPS: https, NoProxy: noProxy}
	if caFile != "" {
		if mode != "explicit" {
			return pooloutbound.Resolved{}, fmt.Errorf("caFile requires proxy mode explicit")
		}
		b, err := os.ReadFile(caFile)
		if err != nil {
			return pooloutbound.Resolved{}, fmt.Errorf("read pool additional trust: %w", err)
		}
		i.CAPEM = string(b)
	}
	return pooloutbound.Resolve(i, os.LookupEnv)
}

func (c *Config) resolvePoolOutbound(configured func(string) bool) error {
	c.PoolOutbound.Configured = false
	p := c.Pools.Proxy
	for _, key := range []string{"mode", "http", "https", "noProxy", "caFile"} {
		c.PoolOutbound.Configured = c.PoolOutbound.Configured || configured("pools.proxy."+key)
	}
	if p.Mode != "explicit" {
		for _, key := range []string{"http", "https", "noProxy", "caFile"} {
			if configured("pools.proxy." + key) {
				return fmt.Errorf("pools.proxy.%s requires mode explicit", key)
			}
		}
	}
	var err error
	c.PoolOutbound.Global, err = resolvePoolProxy(p.Mode, p.HTTP, p.HTTPS, p.NoProxy, p.CAFile)
	if err != nil {
		return fmt.Errorf("pools.proxy: %w", err)
	}
	c.PoolOutbound.GlobalMode = p.Mode
	c.PoolOutbound.Modes = map[string]string{}
	c.PoolOutbound.ByPool = map[string]pooloutbound.Resolved{}
	for id, v := range c.Pools.Overrides {
		if !strings.HasPrefix(id, "pool_") {
			return fmt.Errorf("pools.overrides: selectors must be immutable pool IDs")
		}
		p := v.Proxy
		if p.Mode != "explicit" {
			for _, key := range []string{"http", "https", "noProxy", "caFile"} {
				if configured("pools.overrides." + id + ".proxy." + key) {
					return fmt.Errorf("pools.overrides.%s.proxy.%s requires mode explicit", id, key)
				}
			}
		}
		r, err := resolvePoolProxy(p.Mode, p.HTTP, p.HTTPS, p.NoProxy, p.CAFile)
		if err != nil {
			return fmt.Errorf("pools.overrides.%s.proxy: %w", id, err)
		}
		c.PoolOutbound.Modes[id] = p.Mode
		c.PoolOutbound.ByPool[id] = r
	}
	return nil
}
