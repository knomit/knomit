package main

import "net/url"

// redactURL removes secrets from a URL so it can be printed, logged, or
// committed.
//
// For http(s) the WHOLE userinfo goes: an access token may appear as either the
// username (`https://TOKEN@host`) or the password (`https://user:TOKEN@host`),
// and we cannot tell which. For ssh the username is a login name rather than a
// secret — dropping it would publish a source URL that no longer resolves — so
// only a password is removed.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	switch {
	case u.Scheme == "http" || u.Scheme == "https":
		u.User = nil
	default:
		if _, hasPassword := u.User.Password(); !hasPassword {
			return raw
		}
		u.User = url.User(u.User.Username())
	}
	return u.String()
}
