package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// removedKey is a configuration key or environment variable knomit used to
// read and no longer does. Finding one stops startup: an ignored key looks
// exactly like a setting that works, and for a security setting that is the
// wrong failure.
type removedKey struct {
	Path        string // dotted TOML path, e.g. "oauth.idp"; a table matches every key under it
	Env         string // environment variable name, when the setting had one
	Replacement string // what to do instead, shown to the operator
}

// removedKeys lists every removed setting. There are no compatibility aliases.
var removedKeys = []removedKey{
	{
		Path:        "oauth.idp",
		Replacement: "GitHub sign-in was removed; approve agents with `knomit oauth approve` (later `knomit auth approve`)",
	},
	{
		Env:         "KNOMIT_OAUTH_IDP_CLIENT_SECRET",
		Replacement: "GitHub sign-in was removed; unset it and approve agents with `knomit oauth approve` (later `knomit auth approve`)",
	},
}

// RemovedKeyError is what Load returns when knomit.toml or the environment
// carries a removed setting.
type RemovedKeyError struct {
	Key         string // "[oauth.idp]" for a TOML table or key, the variable name for an env var
	Replacement string
}

func (e *RemovedKeyError) Error() string {
	return fmt.Sprintf("config: %s was removed: %s", e.Key, e.Replacement)
}

// checkRemovedTOML reports the first removed TOML key present in md.
func checkRemovedTOML(md toml.MetaData) error {
	for _, rk := range removedKeys {
		if rk.Path == "" {
			continue
		}
		for _, k := range md.Keys() {
			if s := k.String(); s == rk.Path || strings.HasPrefix(s, rk.Path+".") {
				return &RemovedKeyError{Key: "[" + rk.Path + "]", Replacement: rk.Replacement}
			}
		}
	}
	return nil
}

// checkRemovedEnv reports the first removed environment variable that is set.
func checkRemovedEnv() error {
	for _, rk := range removedKeys {
		if rk.Env == "" {
			continue
		}
		if _, ok := os.LookupEnv(rk.Env); ok {
			return &RemovedKeyError{Key: rk.Env, Replacement: rk.Replacement}
		}
	}
	return nil
}
