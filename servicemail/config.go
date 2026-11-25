package servicemail

import (
	"strconv"
	"strings"

	"github.com/NeoLabs-Systems/neolabs-go/runtimecfg"
)

// Config is the service-email setup read from <PREFIX>_EMAIL_* variables.
type Config struct {
	Enabled bool // SMTP host and sender are configured
	Missing []string

	Host               string
	Port               int
	Secure             bool // implicit TLS (port 465 style)
	RequireTLS         bool // STARTTLS must succeed
	RejectUnauthorized bool // verify the server certificate
	User, Pass         string
	From, ReplyTo      string

	BrandName  string
	SupportURL string
	PublicURL  string

	TokenTTLHours        float64
	NotifyUnusualLogin   bool
	NotifyAccountChanges bool

	RequireSignupConfirmation bool
	RequireChangeConfirmation bool
}

// LoadConfig reads the configuration. prefix is e.g. "NEOAGENT" (variables
// NEOAGENT_EMAIL_SMTP_HOST, ...), defaultBrand names the product and
// defaultPublicURL is used when <PREFIX>_EMAIL_PUBLIC_URL is unset.
func LoadConfig(prefix string, getenv func(string) string, defaultBrand, defaultPublicURL string) Config {
	get := func(name string) string { return strings.TrimSpace(getenv(prefix + "_EMAIL_" + name)) }
	flag := func(name string, def bool) bool { return runtimecfg.Bool(get(name), def) }
	number := func(name string, def float64) float64 {
		v, err := strconv.ParseFloat(get(name), 64)
		if err != nil || v <= 0 {
			return def
		}
		return v
	}

	c := Config{Host: get("SMTP_HOST"), From: get("FROM")}
	c.Port = int(number("SMTP_PORT", 587))
	c.Secure = flag("SMTP_SECURE", c.Port == 465)
	c.RequireTLS = flag("SMTP_REQUIRE_TLS", !c.Secure)
	c.RejectUnauthorized = flag("SMTP_REJECT_UNAUTHORIZED", true)
	c.User, c.Pass = get("SMTP_USER"), get("SMTP_PASS")
	c.ReplyTo = get("REPLY_TO")
	if c.Host == "" {
		c.Missing = append(c.Missing, prefix+"_EMAIL_SMTP_HOST")
	}
	if c.From == "" {
		c.Missing = append(c.Missing, prefix+"_EMAIL_FROM")
	}
	c.Enabled = len(c.Missing) == 0

	c.BrandName = get("BRAND_NAME")
	if c.BrandName == "" {
		c.BrandName = defaultBrand
	}
	c.SupportURL = get("SUPPORT_URL")
	c.PublicURL = strings.TrimRight(get("PUBLIC_URL"), "/")
	if c.PublicURL == "" {
		c.PublicURL = strings.TrimRight(defaultPublicURL, "/")
	}
	c.TokenTTLHours = number("TOKEN_TTL_HOURS", 24)
	c.NotifyUnusualLogin = flag("NOTIFY_UNUSUAL_LOGIN", true)
	c.NotifyAccountChanges = flag("NOTIFY_ACCOUNT_CHANGES", true)
	c.RequireSignupConfirmation = c.Enabled && flag("REQUIRE_SIGNUP_CONFIRMATION", true)
	c.RequireChangeConfirmation = c.Enabled && flag("REQUIRE_EMAIL_CHANGE_CONFIRMATION", true)
	return c
}
