// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

// Package config holds the run configuration, the store bootstrap block and the interest profile.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	profiledomain "github.com/DanielBlei/rabbithole/internal/profile"
	"github.com/DanielBlei/rabbithole/internal/rank"
)

// Duration is a time.Duration that unmarshals from YAML strings, additionally
// accepting a "d" (days) suffix on top of Go's standard units, e.g. "14d",
// "7d", "168h", "36h", "1h30m". The standard library has no day unit.
type Duration time.Duration

// UnmarshalYAML parses a duration string such as "14d" or "168h".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("since must be a duration string like 14d or 168h: %w", err)
	}
	parsed, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a standard time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders the underlying time.Duration.
func (d Duration) String() string { return time.Duration(d).String() }

// Short renders the duration the way it is written in config — "7d", "36h",
// "90m" — rather than Go's "168h0m0s". This is the form the store persists and
// the export emits, so a window survives the round trip looking like what was
// typed instead of expanding into hours.
func (d Duration) Short() string {
	std := time.Duration(d)
	switch {
	case std == 0:
		return "0s"
	case std%(24*time.Hour) == 0:
		return strconv.Itoa(int(std/(24*time.Hour))) + "d"
	case std%time.Hour == 0:
		return strconv.Itoa(int(std/time.Hour)) + "h"
	case std%time.Minute == 0:
		return strconv.Itoa(int(std/time.Minute)) + "m"
	default:
		return std.String()
	}
}

// MarshalYAML writes the duration back as the short string it was parsed from.
// Without this a Duration marshals as its raw nanosecond count, which the
// exported feed set would not be able to read back in.
func (d Duration) MarshalYAML() (any, error) { return d.Short(), nil }

// ParseDuration parses durations with an optional "d" (days) suffix, falling
// back to time.ParseDuration for all standard units (h, m, s, …).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid day duration %q: want an integer before 'd' (e.g. 14d)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: use a 'd' or 'h' suffix (e.g. 14d, 168h)", s)
	}
	return dur, nil
}

// InferenceConfig configures the scoring backend (the model and how to reach it).
type InferenceConfig struct {
	Provider string `yaml:"provider"` // ollama | openai | vllm | heuristic
	Host     string `yaml:"host"`     // inference server URL
	Model    string `yaml:"model"`    // chat model name
	APIKey   string `yaml:"api_key"`  // optional bearer token
	Think    *bool  `yaml:"think"`    // model reasoning during scoring; defaults true when not set in the config
	Effort   string `yaml:"effort"`   // openai reasoning_effort while thinking: low | medium | high | xhigh

	// StructuredOutput sends the verdict schema for the server to enforce; defaults true.
	// false leaves the shape to the system prompt, for endpoints whose guided decoding garbles it.
	StructuredOutput *bool `yaml:"structured_output"`

	// SystemPrompt: unset takes inference's built-in default, text replaces it, false sends none.
	SystemPrompt SystemPromptSetting `yaml:"system_prompt"`

	// How items are batched through the model. BatchSize also sizes the token
	// budget: ModelTuning.Budget multiplies TokensPerItem by it.
	BatchSize   int `yaml:"batch_size"`   // items per scoring request
	MaxParallel int `yaml:"max_parallel"` // concurrent scoring requests in flight

	// ModelTuning carries the decoding limits.
	// Omit the block, or any field in it, to take rank's defaults.
	ModelTuning rank.ModelTuning `yaml:"model_tuning"`
}

// SystemPromptSetting is the inference.system_prompt value: the prompt text, false
// (disabled), or unset (the built-in default). A config file may name a prompt file
// instead of the text; File.InlineSystemPrompt replaces the path with the file's
// contents before anything is stored.
type SystemPromptSetting struct {
	Text     string
	Disabled bool
}

// UnmarshalYAML accepts text or the literal false; true is rejected.
func (s *SystemPromptSetting) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag == "!!bool" {
		var b bool
		if err := value.Decode(&b); err != nil {
			return err
		}
		if b {
			return fmt.Errorf("system_prompt: true has no meaning; use the prompt text, false, or omit it")
		}
		s.Disabled = true
		return nil
	}
	return value.Decode(&s.Text)
}

// LooksLikePath reports whether a system_prompt value reads as a file path rather
// than prompt text: one line that has no whitespace, or that starts with /, ./,
// ../ or ~/. Prompt text is normally several words, so it never matches.
func LooksLikePath(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, "\r\n") {
		return false
	}
	if !strings.ContainsAny(v, " \t") {
		return true
	}
	for _, p := range []string{"/", "./", "../", "~/"} {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// FileInference is the inference block of a config file: the stored settings plus
// the keys that are accepted and dropped.
type FileInference struct {
	InferenceConfig `yaml:",inline"`

	// Summary was a placeholder for a separate summarization backend. It is
	// accepted so old files import, and ignored.
	Summary map[string]any `yaml:"summary"`
}

// FileIngest is the ingest block of a config file. Feeds is a legacy import
// directive: the path a one-time feed seed is read from.
type FileIngest struct {
	Since     Duration `yaml:"since"`
	DigestDir string   `yaml:"digest_dir"`
	Feeds     string   `yaml:"feeds"`
}

// StoreConfig configures item persistence. Exactly one of the two fields is
// set, and which one picks the engine: a path means SQLite, a URL means
// Postgres. There is no separate driver setting, so nothing can disagree.
type StoreConfig struct {
	DBPath string `yaml:"db_path"` // sqlite database path
	URL    string `yaml:"url"`     // postgres connection url; the password may be a $NAME reference

	// MaxConns caps the Postgres pool. The default is already small enough for a
	// hosted free tier; this is for going lower when a database is shared with
	// other applications, not for going high.
	MaxConns int `yaml:"max_conns"`

	// ConnMaxLifetime is how long one pooled connection may live. Lower it when
	// something in front of the database recycles server connections; see
	// docs/configuration.md. Zero keeps the default.
	ConnMaxLifetime Duration `yaml:"conn_max_lifetime"`
}

// DBPasswordEnv supplies the Postgres password, overriding store.url's.
const DBPasswordEnv = "RABBITHOLE_DB_PASSWORD"

// LibraryURLEnv overrides the directory the source library is fetched from.
// Empty means the project default; "off" turns the library off.
const LibraryURLEnv = "RABBITHOLE_LIBRARY_URL"

// defaultSSLMode is applied when the URL names none. It encrypts without
// verifying the certificate, which is the one mode that works out of the box on
// the hosted databases the docs point people at: Supabase signs its database
// certificate with its own CA, so verify-full cannot connect until the operator
// supplies that bundle. verify-full stays the right answer and is one URL
// parameter away; openStore warns when this default leaves a session unverified
// over anything but loopback, so the weaker default is loud rather than silent.
const defaultSSLMode = "require"

// sslVerifyFull and sslVerifyCA are the two modes that check who answered.
const (
	sslVerifyFull = "verify-full"
	sslVerifyCA   = "verify-ca"
)

// IsPostgres reports whether the store is configured for Postgres.
func (s StoreConfig) IsPostgres() bool { return s.URL != "" }

// Postgres is a resolved connection: DSN carries the password and must never
// be logged, Label is the same URL with the password removed and is what
// errors and log lines show.
type Postgres struct {
	DSN   string
	Label string

	// PasswordInConfigFile records that store.url carries a password, in either
	// of the two forms the drivers take. It stays true when DBPasswordEnv
	// overrides the value, because what deserves a warning is a secret sitting in
	// a file that gets copied around and shown in Settings, Advanced — not which
	// of the two sources won.
	PasswordInConfigFile bool

	// HasPassword is false when neither source supplied one, which is legal
	// (a server can trust the client) but is the likeliest reason a connection
	// is refused, so callers say so when one is.
	HasPassword bool

	// SSLMode is the mode that will actually be used, after the default is
	// applied, and Loopback says whether the host is reached over the machine's
	// own loopback. Together they let a caller warn about the weak modes without
	// reparsing the DSN: sslmode=require encrypts but does not verify the server,
	// and this store carries the auth signing key, so an intercepted session leaks
	// more than the rows. Over loopback nobody but this machine can intercept it.
	SSLMode  string
	Loopback bool
}

// TLSVerified reports whether the server's certificate gets checked. False is
// not automatically wrong — it is what makes a Supabase or RDS install work on
// the first try — but it is a gap the operator should know they are running
// with, so callers say so.
func (p Postgres) TLSVerified() bool {
	return p.SSLMode == sslVerifyFull || p.SSLMode == sslVerifyCA
}

// TLSEncrypted reports whether the connection is encrypted at all. sslmode
// values below `require` fall back to plain text, which is a different warning
// than unverified encryption and should not be dressed up as the same one.
// `prefer` belongs with them rather than above them: it asks for TLS and then
// falls back without a word when the server declines, so nothing about the
// session is guaranteed even though the operator did ask for encryption.
func (p Postgres) TLSEncrypted() bool {
	switch p.SSLMode {
	case "disable", "allow", "prefer":
		return false
	default:
		return true
	}
}

// bracedEnvRef matches ${NAME}, which url.Parse rejects in userinfo. It only
// rewrites the braces away; parseRef still decides what counts as a name.
var bracedEnvRef = regexp.MustCompile(`\$\{(` + refNamePattern + `)\}`)

// ResolvePostgres builds the connection from store.url plus DBPasswordEnv. The
// environment wins over a password in the URL, so an exported variable can
// override a checked-in file without editing it. A URL password starting with
// '$' is an environment reference, resolved like api_key.
func (s StoreConfig) ResolvePostgres() (Postgres, error) {
	u, err := url.Parse(bracedEnvRef.ReplaceAllString(s.URL, "$$$1"))
	if err != nil {
		// url.Error quotes the whole url it failed on, which here may hold a
		// password — and a mistyped DSN is the first thing a new install gets
		// wrong, so this text reaches a terminal and a journal. Report the reason
		// alone; the label built below is what is safe to repeat.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return Postgres{}, fmt.Errorf("store.url is not a valid url: %v", urlErr.Err)
		}
		return Postgres{}, fmt.Errorf("store.url is not a valid url: %v", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
	default:
		return Postgres{}, fmt.Errorf("store.url scheme is %q, want postgres", u.Scheme)
	}

	user := u.User.Username()
	inURL, hasInURL := u.User.Password()

	// The drivers also take the password as a query parameter. Lift it into
	// the userinfo so there is one path from here on, and so it cannot slip
	// past the warning below by arriving in the other form.
	q := u.Query()
	if param := q.Get("password"); param != "" {
		inURL, hasInURL = param, true
		q.Del("password")
		u.RawQuery = q.Encode()
	}

	// The file's secret is worth reporting whatever happens to it next, so this
	// is captured before the environment is allowed to override it.
	isRef := strings.HasPrefix(inURL, "$")
	inConfigFile := hasInURL && !isRef

	password := inURL
	if env := os.Getenv(DBPasswordEnv); env != "" {
		password = env
	} else if isRef {
		if password, err = expandEnvRef("store.url password", inURL); err != nil {
			return Postgres{}, err
		}
	}
	// url.UserPassword escapes, so a password holding @ / # or ? cannot
	// corrupt the DSN the way string concatenation would.
	switch {
	case password != "":
		u.User = url.UserPassword(user, password)
	case user != "":
		u.User = url.User(user)
	}

	if q.Get("sslmode") == "" {
		q.Set("sslmode", defaultSSLMode)
		u.RawQuery = q.Encode()
	}
	sslMode := q.Get("sslmode")
	loopback := loopbackHost(u.Hostname())

	labelURL := *u
	if user != "" {
		labelURL.User = url.User(user)
	} else {
		labelURL.User = nil
	}
	return Postgres{
		DSN:                  u.String(),
		Label:                labelURL.String(),
		PasswordInConfigFile: inConfigFile,
		HasPassword:          password != "",
		SSLMode:              sslMode,
		Loopback:             loopback,
	}, nil
}

// loopbackHost reports whether the database is reached over the machine's own
// loopback, where intercepting the connection already needs a shell on it.
// Everything else — a container network, another host on the LAN, a hosted
// database — is a path someone could be sitting on.
func loopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// Defaults (Ollama on localhost).
const (
	defaultProvider    = "ollama"
	defaultHost        = "http://localhost:11434"
	defaultModel       = "qwen3.5:4b"
	defaultBatchSize   = 1
	defaultMaxParallel = 1
	defaultEffort      = "medium"
)

// DefaultSince is the lookback window a feed gets when neither it nor the feed
// defaults set one.
const DefaultSince = 7 * 24 * time.Hour

// efforts are the reasoning_effort levels inference.effort accepts. Lower ones
// belong to think: false, which probes the model's own lowest.
var efforts = []string{"low", "medium", "high", "xhigh"}

// Validate checks the store block: exactly one target, a sane pool, and pool
// settings only beside a Postgres url. It is what bootstrap runs on whichever
// location it resolved.
func (s StoreConfig) Validate() error {
	switch {
	case s.DBPath == "" && s.URL == "":
		return fmt.Errorf("store needs either db_path (sqlite) or url (postgres)")
	case s.DBPath != "" && s.URL != "":
		return fmt.Errorf("store has both db_path and url; keep the one for the engine you want")
	}
	if s.MaxConns < 0 {
		return fmt.Errorf("store.max_conns must not be negative, got %d", s.MaxConns)
	}
	if s.ConnMaxLifetime < 0 {
		return fmt.Errorf("store.conn_max_lifetime must not be negative, got %s", s.ConnMaxLifetime)
	}
	if s.IsPostgres() {
		if _, err := s.ResolvePostgres(); err != nil {
			return err
		}
	} else if s.MaxConns != 0 || s.ConnMaxLifetime != 0 {
		// Naming them beside a sqlite path would otherwise look like it did
		// something: there is no pool to shape, only the pragmas in its DSN.
		return fmt.Errorf("store.max_conns and store.conn_max_lifetime apply to store.url, not db_path")
	}
	return nil
}

// parseRef reads a whole $NAME or ${NAME} reference. ok is false for anything
// else, including a '$' elsewhere in the value or a malformed name.
func parseRef(value string) (name string, ok bool) {
	switch {
	case strings.HasPrefix(value, "${"):
		if !strings.HasSuffix(value, "}") || len(value) < 4 {
			return "", false
		}
		name = value[2 : len(value)-1]
	case strings.HasPrefix(value, "$"):
		name = value[1:]
	default:
		return "", false
	}
	if !refNameRE.MatchString(name) {
		return "", false
	}
	return name, true
}

// refNamePattern is the shape of an environment variable name in a reference.
const refNamePattern = `[A-Za-z_][A-Za-z0-9_]*`

var refNameRE = regexp.MustCompile(`^` + refNamePattern + `$`)

// expandEnvRef resolves one api_key value. Errors never include the value.
func expandEnvRef(field, value string) (string, error) {
	if !strings.Contains(value, "$") {
		return value, nil
	}
	name, ok := parseRef(value)
	if !ok {
		return "", fmt.Errorf("%s: a '$' is only allowed as a whole $NAME or ${NAME} reference", field)
	}
	resolved, set := os.LookupEnv(name)
	if !set || resolved == "" {
		return "", fmt.Errorf("%s: $%s is not set or empty", field, name)
	}
	return resolved, nil
}

// stripHTMLComments drops <!-- --> blocks, so comments never reach the model
func stripHTMLComments(md string) string {
	return profiledomain.Clean(md)
}

// loadTrimmedFile reads path and strips HTML comments, so notes to yourself
// (in a profile or a system prompt override) never reach the model.
func loadTrimmedFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return stripHTMLComments(string(raw)), nil
}

// LoadProfileFile reads the interest-profile markdown at path; an empty path
// returns the built-in default.
func LoadProfileFile(path string) (string, error) {
	if path == "" {
		return profiledomain.DefaultContent, nil
	}
	profile, err := loadTrimmedFile(path)
	if err != nil {
		return "", fmt.Errorf("read profile %q: %w", path, err)
	}
	// The shipped template is mostly an HTML comment, so a file that looks
	// filled in can strip to nothing. Without it every score is arbitrary while
	// the run still looks like it worked.
	if strings.TrimSpace(profile) == "" {
		return "", fmt.Errorf(
			"profile %q is empty; it is required in order to make meaningful suggestions",
			path,
		)
	}
	return profile, nil
}

// EffectiveSystemPrompt resolves inference.system_prompt: disabled returns "", text returns
// itself, and unset returns the built-in default. The heuristic provider never looks at a
// system prompt, so it always gets "".
func (c InferenceConfig) EffectiveSystemPrompt() string {
	switch {
	case c.Provider == "heuristic" || c.SystemPrompt.Disabled:
		return ""
	case c.SystemPrompt.Text != "":
		return c.SystemPrompt.Text
	default:
		return rank.DefaultSystemPrompt
	}
}
