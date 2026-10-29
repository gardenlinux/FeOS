// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"errors"
	"strings"
)

// RegistryCredentials are a create's credentials for its image's registry
// (ADR 0025): a login as `docker login` takes it, a username and a
// password or token. The zero value is anonymous. They are sealed at rest
// and never returned, and no pod sees them: the guest pulls with a token
// the registry gave for them.
type RegistryCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Anonymous reports whether there are no credentials.
func (c RegistryCredentials) Anonymous() bool { return c.Username == "" && c.Password == "" }

// maxCredential bounds a username and a password.
const maxCredential = 4096

// Check refuses credentials that are half given, too long, or carry
// characters a header cannot.
func (c RegistryCredentials) Check() error {
	if c.Anonymous() {
		return nil
	}
	if c.Username == "" || c.Password == "" {
		return errors.New("image credentials need a username and a password")
	}
	if len(c.Username) > maxCredential || len(c.Password) > maxCredential {
		return errors.New("an image credential has at most 4096 bytes")
	}
	if strings.ContainsAny(c.Username, ":\r\n\x00") || strings.ContainsAny(c.Password, "\r\n\x00") {
		return errors.New("an image credential's username has no colon, and neither has a line break or NUL")
	}
	return nil
}

// Resolve is the login c names, its secrets read by secret (a name, at
// its latest version); nil c is anonymous.
func (c *TemplateCredentials) Resolve(secret func(name string) (string, error)) (RegistryCredentials, error) {
	if c == nil {
		return RegistryCredentials{}, nil
	}
	value := func(v TemplateValue) (string, error) {
		if v.Secret == "" {
			return v.Value, nil
		}
		return secret(v.Secret)
	}
	user, err := value(c.Username)
	if err != nil {
		return RegistryCredentials{}, err
	}
	password, err := value(c.Password)
	if err != nil {
		return RegistryCredentials{}, err
	}
	return RegistryCredentials{Username: user, Password: password}, nil
}
