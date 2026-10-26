package connector

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	up "go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
)

//go:embed example-config.yaml
var ExampleConfig string

// Config is the network section of the bridge config.
type Config struct {
	Servers             []ServerConfig `yaml:"servers"`
	GroupSpaces         bool           `yaml:"group_spaces"`
	DisplaynameTemplate string         `yaml:"displayname_template"`
	SyncPresence        bool           `yaml:"sync_presence"`
	KeepLoginsOnline    bool           `yaml:"keep_logins_online"`
	CallNotices         bool           `yaml:"call_notices"`
	ThreadEcho          string         `yaml:"thread_echo"`
	AllowPasswordLogin  bool           `yaml:"allow_password_login"`
	StrictServerVersion bool           `yaml:"strict_server_version"`

	Metrics struct {
		Enabled bool   `yaml:"enabled"`
		Listen  string `yaml:"listen"`
	} `yaml:"metrics"`

	GhostBots struct {
		Enabled             bool   `yaml:"enabled"`
		LoginPrefix         string `yaml:"login_prefix"`
		DisplaynameTemplate string `yaml:"displayname_template"`
	} `yaml:"ghost_bots"`

	displaynameTemplate         *template.Template
	ghostBotDisplaynameTemplate *template.Template
}

// ServerConfig is one Chatto server served by the bridge.
type ServerConfig struct {
	// ID is part of every network ID of the server and must never change.
	ID  string `yaml:"id"`
	URL string `yaml:"url"`
}

var serverIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)

// Server returns the config of a server, or nil if it is not configured.
func (c *Config) Server(id string) *ServerConfig {
	for i := range c.Servers {
		if c.Servers[i].ID == id {
			return &c.Servers[i]
		}
	}
	return nil
}

type umConfig Config

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	if err := node.Decode((*umConfig)(c)); err != nil {
		return err
	}
	return c.PostProcess()
}

// PostProcess compiles templates. It is called after unmarshalling.
func (c *Config) PostProcess() error {
	var err error
	if c.DisplaynameTemplate == "" {
		c.DisplaynameTemplate = "{{or .DisplayName .Login}}"
	}
	c.displaynameTemplate, err = template.New("displayname").Parse(c.DisplaynameTemplate)
	if err != nil {
		return fmt.Errorf("invalid displayname_template: %w", err)
	}
	if c.GhostBots.DisplaynameTemplate == "" {
		c.GhostBots.DisplaynameTemplate = "{{.DisplayName}}"
	}
	c.ghostBotDisplaynameTemplate, err = template.New("ghost_bot_displayname").Parse(c.GhostBots.DisplaynameTemplate)
	if err != nil {
		return fmt.Errorf("invalid ghost_bots.displayname_template: %w", err)
	}
	return nil
}

// Validate checks the config for errors that would prevent the bridge from working.
func (c *Config) Validate() error {
	if len(c.Servers) == 0 {
		return fmt.Errorf("network.servers must list at least one Chatto server")
	}
	seen := make(map[string]bool, len(c.Servers))
	for i, srv := range c.Servers {
		if !serverIDPattern.MatchString(srv.ID) {
			return fmt.Errorf("network.servers[%d].id must be 1-16 characters of a-z, 0-9 and -, not %q", i, srv.ID)
		}
		if seen[srv.ID] {
			return fmt.Errorf("network.servers[%d].id %q is used twice", i, srv.ID)
		}
		seen[srv.ID] = true
		if _, err := chattoclient.ParseServerURL(srv.URL); err != nil {
			return fmt.Errorf("network.servers[%d].url: %w", i, err)
		}
	}
	switch c.ThreadEcho {
	case "", "skip", "notice":
	default:
		return fmt.Errorf("network.thread_echo must be skip or notice, not %q", c.ThreadEcho)
	}
	if c.Metrics.Enabled && c.Metrics.Listen == "" {
		return fmt.Errorf("network.metrics.listen is required when metrics are enabled")
	}
	if c.GhostBots.Enabled && len(c.GhostBots.LoginPrefix) > 16 {
		return fmt.Errorf("network.ghost_bots.login_prefix must be at most 16 characters")
	}
	return nil
}

// DisplaynameParams are the fields available in displayname_template.
type DisplaynameParams struct {
	DisplayName string
	Login       string
	ID          string
	IsBot       bool
}

// FormatDisplayname renders displayname_template.
func (c *Config) FormatDisplayname(p DisplaynameParams) string {
	var sb strings.Builder
	if err := c.displaynameTemplate.Execute(&sb, &p); err != nil {
		return p.Login
	}
	name := strings.TrimSpace(sb.String())
	if name == "" {
		return p.ID
	}
	return name
}

// GhostBotParams are the fields available in ghost_bots.displayname_template.
type GhostBotParams struct {
	DisplayName string
	UserID      string
}

// FormatGhostBotDisplayname renders ghost_bots.displayname_template, cut to
// Chatto's 32 character limit.
func (c *Config) FormatGhostBotDisplayname(p GhostBotParams) string {
	var sb strings.Builder
	if err := c.ghostBotDisplaynameTemplate.Execute(&sb, &p); err != nil {
		return truncateRunes(p.DisplayName, 32)
	}
	return truncateRunes(strings.TrimSpace(sb.String()), 32)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.List, "servers")
	helper.Copy(up.Bool, "group_spaces")
	helper.Copy(up.Str, "displayname_template")
	helper.Copy(up.Bool, "sync_presence")
	helper.Copy(up.Bool, "keep_logins_online")
	helper.Copy(up.Bool, "call_notices")
	helper.Copy(up.Str, "thread_echo")
	helper.Copy(up.Bool, "allow_password_login")
	helper.Copy(up.Bool, "strict_server_version")
	helper.Copy(up.Bool, "metrics", "enabled")
	helper.Copy(up.Str, "metrics", "listen")
	helper.Copy(up.Bool, "ghost_bots", "enabled")
	helper.Copy(up.Str, "ghost_bots", "login_prefix")
	helper.Copy(up.Str, "ghost_bots", "displayname_template")
}

func (c *Connector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &c.Config, up.SimpleUpgrader(upgradeConfig)
}

func (c *Connector) ValidateConfig() error {
	return c.Config.Validate()
}
