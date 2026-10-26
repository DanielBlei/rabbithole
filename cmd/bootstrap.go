// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
	"github.com/DanielBlei/rabbithole/internal/web"
)

const (
	// dbEnv names the store location when --db is not given.
	dbEnv = "RABBITHOLE_DB"

	defaultEnvFile = "./.env"

	// storeFile is the SQLite file name inside the per-user data dir.
	storeFile = "rabbithole.db"

	// legacyDefaultDB and legacyInstallerDB are where earlier releases kept the
	// default store, both relative to the working directory. They are still read,
	// with a warning, until legacyUntil.
	legacyDefaultDB   = "./data/rabbithole.db"
	legacyInstallerDB = "./rabbithole/data/rabbithole.db"
	legacyUntil       = "v0.4.0"
)

// storeOrigin says where the store location came from, for the boot log.
type storeOrigin string

const (
	originFlag    storeOrigin = "flag"
	originEnv     storeOrigin = "env"
	originFile    storeOrigin = "file"
	originDefault storeOrigin = "default"
	originLegacy  storeOrigin = "legacy"
)

// storeInputs are the places a store location can come from.
type storeInputs struct {
	Flag            string // --db
	Env             string // RABBITHOLE_DB
	MaxConns        int    // --db-max-conns
	ConnMaxLifetime config.Duration
	File            *config.StoreConfig // the store block of the config file, nil without one
}

// storeAt reads a --db or RABBITHOLE_DB value: a postgres:// or postgresql://
// URL names Postgres, anything else is a SQLite path.
func storeAt(location string) config.StoreConfig {
	if strings.HasPrefix(location, "postgres://") || strings.HasPrefix(location, "postgresql://") {
		return config.StoreConfig{URL: location}
	}
	return config.StoreConfig{DBPath: location}
}

// storeEnv is what the default store location depends on, injected so the
// resolver is a pure function.
type storeEnv struct {
	getenv func(string) string
	home   string // "" when the home directory cannot be found
	goos   string
	exists func(path string) bool
}

// userStoreEnv is the real environment of this process.
func userStoreEnv() storeEnv {
	home, _ := os.UserHomeDir()
	return storeEnv{getenv: os.Getenv, home: home, goos: runtime.GOOS, exists: fileExists}
}

// defaultStore is where a run with no named store keeps its SQLite file.
type defaultStore struct {
	Path    string   // what is opened
	PerUser string   // the per-user path, whether or not it is the one in use
	Legacy  bool     // Path is a legacy location
	Exists  bool     // Path is already there
	Ignored []string // legacy stores that exist but are not used
}

// perUserDir is the rabbithole directory under the per-user data home: the XDG
// data home on Linux (a relative value is ignored, per the spec), Application
// Support on macOS. dataHome is the directory that holds it.
func perUserDir(env storeEnv) (dir, dataHome string, err error) {
	if env.goos != "darwin" {
		if x := env.getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
			return filepath.Join(x, "rabbithole"), filepath.Clean(x), nil
		}
	}
	if env.home == "" {
		return "", "", errors.New(
			"cannot find a home directory for the default store; set " + dbEnv + " or pass --db")
	}
	dataHome = filepath.Join(env.home, ".local", "share")
	if env.goos == "darwin" {
		dataHome = filepath.Join(env.home, "Library", "Application Support")
	}
	return filepath.Join(dataHome, "rabbithole"), dataHome, nil
}

// locateDefaultStore picks the default store: the per-user file if it exists,
// else a legacy file in the working directory, else the per-user path to create.
// It only looks, never creates.
func locateDefaultStore(env storeEnv) (defaultStore, error) {
	dir, _, err := perUserDir(env)
	if err != nil {
		return defaultStore{}, err
	}
	d := defaultStore{PerUser: filepath.Join(dir, storeFile)}
	var present []string
	for _, p := range []string{legacyDefaultDB, legacyInstallerDB} {
		if env.exists(p) {
			present = append(present, p)
		}
	}
	switch {
	case env.exists(d.PerUser):
		d.Path, d.Exists, d.Ignored = d.PerUser, true, present
	case len(present) > 0:
		d.Path, d.Legacy, d.Exists, d.Ignored = present[0], true, true, present[1:]
	default:
		d.Path = d.PerUser
	}
	return d, nil
}

// resolveStoreConfig applies the precedence --db flag, RABBITHOLE_DB, the file's
// store block, then the default location, and validates the result. The
// default's details come back for the boot log and the create question.
func resolveStoreConfig(in storeInputs, env storeEnv) (config.StoreConfig, storeOrigin, defaultStore, error) {
	var (
		sc     config.StoreConfig
		origin storeOrigin
		def    defaultStore
	)
	switch {
	case in.Flag != "":
		sc, origin = storeAt(in.Flag), originFlag
	case in.Env != "":
		sc, origin = storeAt(in.Env), originEnv
	case in.File != nil && *in.File != (config.StoreConfig{}):
		sc, origin = *in.File, originFile
	default:
		var err error
		if def, err = locateDefaultStore(env); err != nil {
			return config.StoreConfig{}, "", defaultStore{}, err
		}
		sc, origin = config.StoreConfig{DBPath: def.Path}, originDefault
		if def.Legacy {
			origin = originLegacy
		}
	}
	poolFlags := in.MaxConns != 0 || in.ConnMaxLifetime != 0
	if in.MaxConns != 0 {
		sc.MaxConns = in.MaxConns
	}
	if in.ConnMaxLifetime != 0 {
		sc.ConnMaxLifetime = in.ConnMaxLifetime
	}
	if err := sc.Validate(); err != nil {
		if poolFlags && !sc.IsPostgres() {
			return config.StoreConfig{}, "", defaultStore{}, errors.New(
				"--db-max-conns and --db-conn-max-lifetime apply to a postgres:// store, not a SQLite path")
		}
		return config.StoreConfig{}, "", defaultStore{}, err
	}
	return sc, origin, def, nil
}

// tildePath shortens a path under home to ~/..., for people to read. What is
// opened is never shortened.
func tildePath(path, home string) string {
	if home == "" || !filepath.IsAbs(path) {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, strings.TrimRight(home, "/")+"/"); ok {
		return "~/" + rest
	}
	return path
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// loadEnv reads the env file, filling only variables the environment leaves unset.
func loadEnv() (*config.EnvFile, error) {
	env, err := config.LoadEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	switch {
	case !fileExists(env.Path):
		log.Debug().Str("env_file", env.Path).Msg("no env file")
	case env.Loaded() > 0:
		log.Info().Str("env_file", env.Path).Int("variables", env.Loaded()).Msg("env file loaded")
	default:
		log.Debug().Str("env_file", env.Path).Msg("env file loaded, nothing to set")
	}
	return env, nil
}

// boot is the resolved bootstrap: where the store is, and the config file named
// with --config, if any.
type boot struct {
	store    config.StoreConfig
	origin   storeOrigin
	def      defaultStore // set for the default and legacy origins
	home     string
	filePath string // --config; "" without one
	hint     bool   // the default store is created without asking
	asked    bool   // the user agreed to create the default store
}

// resolveBoot picks the store location. The only config file read is the one
// --config names.
func resolveBoot(env storeEnv) (boot, error) {
	b := boot{filePath: configPath, home: env.home}
	in := storeInputs{Flag: dbFlag, Env: os.Getenv(dbEnv), MaxConns: dbMaxConns}
	if dbLifetime != "" {
		d, err := config.ParseDuration(dbLifetime)
		if err != nil {
			return boot{}, fmt.Errorf("--db-conn-max-lifetime: %w", err)
		}
		in.ConnMaxLifetime = config.Duration(d)
	}
	if b.filePath != "" {
		f, err := config.ReadFile(b.filePath)
		if err != nil {
			return boot{}, fmt.Errorf("%s: %w", b.filePath, err)
		}
		_, st, _, _ := f.Split()
		in.File = &st
	}
	var err error
	if b.store, b.origin, b.def, err = resolveStoreConfig(in, env); err != nil {
		return boot{}, err
	}
	return b, nil
}

// session is one command's connection to the store and its run config.
type session struct {
	db  *store.Store
	env *config.EnvFile
	src config.Source

	storeInfo web.StoreInfo // what the settings page shows about the store

	missing string // the SQLite path that does not exist, as shown to people, for an optional session
}

// Close closes the store, if one was opened.
func (s *session) Close() {
	if s.db == nil {
		return
	}
	if err := s.db.Close(); err != nil {
		log.Warn().Err(err).Msg("db close failed")
	}
}

type sessionOpts struct {
	exclusive bool // claim the single-writer guard; only `serve`
	quiet     bool // log the connection at debug unless the store was just created
	noImport  bool // use the file's store block but do not import its settings
	mustExist bool // a SQLite store that is not there is an error, not created
	optional  bool // a SQLite store that is not there yields the defaults, not a new store
}

// openSession is the shared start of every command that uses the store: load the
// env file, resolve the store, open it, and import the config file if one
// applies. Every command goes through it so the redaction and the logging
// cannot be forgotten at one call site.
func openSession(cmd *cobra.Command, o sessionOpts) (*session, error) {
	ctx := cmd.Context()
	env, err := loadEnv()
	if err != nil {
		return nil, err
	}
	b, err := resolveBoot(userStoreEnv())
	if err != nil {
		return nil, err
	}
	logLegacyStore(b)
	if (o.mustExist || o.optional) && !b.store.IsPostgres() && !fileExists(b.store.DBPath) {
		shown := tildePath(b.store.DBPath, b.home)
		if o.mustExist {
			return nil, fmt.Errorf("no store at %s", shown)
		}
		return &session{env: env, src: config.Static(config.RunDefaults()), missing: shown}, nil
	}
	if err := prepareNewStore(ctx, &b); err != nil {
		return nil, err
	}
	db, err := openStore(ctx, b, o)
	if err != nil {
		return nil, err
	}
	s := &session{db: db, env: env, src: db.ConfigSource(), storeInfo: describeStoreInfo(b)}
	if b.filePath != "" && !o.noImport {
		res, err := importConfig(ctx, db, env, b.filePath, importOpts{onStart: true})
		if err != nil {
			s.Close()
			return nil, err
		}
		if r := res.Replaced; r != nil {
			log.Warn().Msgf("config version %d (from %s) replaced by %s; undo with: rabbithole config rollback %d",
				r.Version, replacedSummary(*r), b.filePath, r.Version)
		}
	}
	return s, nil
}

// openStore opens the store b names and logs which one. The resolved Label, never
// the DSN, reaches a log line or an error, because a Postgres URL carries the
// password.
//
// exclusive claims the single-writer guard, which only `serve` passes: a
// one-shot command has to work while a server is up.
func openStore(ctx context.Context, b boot, o sessionOpts) (*store.Store, error) {
	sc := b.store
	label := tildePath(sc.DBPath, b.home)
	var pg config.Postgres
	existed := true
	if sc.IsPostgres() {
		var err error
		if pg, err = sc.ResolvePostgres(); err != nil {
			return nil, err
		}
		label = pg.Label
		if pg.PasswordInConfigFile {
			log.Warn().Msgf("the store URL carries a password; set %s instead so it stays out of"+
				" the process list and any config file", config.DBPasswordEnv)
		}
	} else {
		existed = fileExists(sc.DBPath)
	}
	var (
		db  *store.Store
		err error
	)
	if o.exclusive {
		db, err = store.OpenExclusive(ctx, sc)
	} else {
		db, err = store.OpenFrom(ctx, sc)
	}
	if err != nil {
		if sc.IsPostgres() && !pg.HasPassword && !errors.Is(err, store.ErrStoreInUse) {
			return nil, fmt.Errorf("%w (no password given: set %s)", err, config.DBPasswordEnv)
		}
		return nil, err
	}
	if !existed && b.origin == originDefault {
		restrictNewStore(sc.DBPath)
		if b.asked {
			announceNewStore(label)
		}
	}
	logStoreConnected(b, label, !existed, o, pg)
	return db, nil
}

// logStoreConnected says which store this process opened and on what terms: the
// engine, the target, where the location came from, and on Postgres the sslmode
// with what that mode actually delivers, as fields rather than as prose. A store
// this process just created says "store created" instead. `serve` says it at
// info, since boot is where an operator checks the settings took effect; the
// quiet commands (`items`, `auth`) say it at debug unless they created the
// store, so `items list` stays quiet.
//
// Stated, not warned about. What each sslmode costs is argued once in
// docs/configuration.md#tls, and repeating it every boot reads as alarm about the
// documented default, which trains people to ignore the lines that do mean
// something. The two booleans still carry the whole fact: over loopback
// `verified:false` is nobody else's reach, and `encrypted:false` is visible to
// anyone reading the line.
func logStoreConnected(b boot, label string, created bool, o sessionOpts, pg config.Postgres) {
	level := zerolog.InfoLevel
	if (o.quiet && !created) || b.asked {
		level = zerolog.DebugLevel
	}
	engine := "sqlite"
	if b.store.IsPostgres() {
		engine = "postgres"
	}
	event := log.WithLevel(level).Str("engine", engine).Str("store", label)
	switch {
	case created:
		if b.hint {
			event = event.Str("hint", "set "+dbEnv+" for Postgres or another path")
		} else if b.origin != originDefault {
			event = event.Str("from", string(b.origin))
		}
		event.Msg("store created")
		return
	case b.store.IsPostgres():
		event = event.Str("from", string(b.origin)).
			Str("sslmode", pg.SSLMode).
			Bool("encrypted", pg.TLSEncrypted()).
			Bool("verified", pg.TLSVerified()).
			Bool("loopback", pg.Loopback)
	default:
		event = event.Str("from", string(b.origin))
	}
	event.Msg("store connected")
}

// logLegacyStore warns when the store is a legacy one, and says which legacy
// stores are present but unused.
func logLegacyStore(b boot) {
	if b.origin == originLegacy {
		dir := tildePath(filepath.Dir(b.def.PerUser), b.home) + "/"
		log.Warn().Msgf("using legacy store %s (deprecated, supported until %s); "+
			"move it to %s or pass --db %s to keep it here", b.def.Path, legacyUntil, dir, b.def.Path)
	}
	for _, p := range b.def.Ignored {
		log.Info().Msgf("legacy store %s is ignored; using %s", p, tildePath(b.def.Path, b.home))
	}
}

// legacyConfigFile is where earlier releases read the config from by default.
const legacyConfigFile = "./configs/config.yaml"

// logLegacyConfigFile warns once when the old default config file is present and
// no --config names a file. It is never read: files are explicit imports.
func logLegacyConfigFile() {
	if configPath != "" || !fileExists(legacyConfigFile) {
		return
	}
	log.Warn().Msgf("%s is no longer read automatically; import it once with "+
		"`rabbithole config import %s` or pass --config", legacyConfigFile, legacyConfigFile)
}

// describeStoreInfo is the store as the settings page shows it: the engine, a
// location with the home directory shortened and no password, and where the
// location came from in words.
func describeStoreInfo(b boot) web.StoreInfo {
	info := web.StoreInfo{Engine: "SQLite", Location: tildePath(b.store.DBPath, b.home)}
	if b.store.IsPostgres() {
		info.Engine, info.Location = "Postgres", ""
		if pg, err := b.store.ResolvePostgres(); err == nil {
			info.Location = pg.Label
		}
	}
	switch b.origin {
	case originFlag:
		info.Origin = "the --db flag"
	case originEnv:
		info.Origin = "the " + dbEnv + " variable"
	case originFile:
		info.Origin = "the --config file"
	case originLegacy:
		info.Origin = "an older default location"
	default:
		info.Origin = "the default location"
	}
	return info
}
