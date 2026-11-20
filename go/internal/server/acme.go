// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"

	"github.com/gardenlinux/feos/go/internal/acme"
	"github.com/gardenlinux/feos/go/internal/awsauth"
	"github.com/gardenlinux/feos/go/internal/config"
	"github.com/gardenlinux/feos/go/internal/route53"
	"github.com/gardenlinux/feos/go/internal/seal"
)

// newACME builds the ACME client of the domains' certificates (ADR 0021,
// internal/acme) with --acme-directory and --acme-email; it seals the
// keys with sealer, --secret-key-file's (nil: none, which config refuses
// with a directory). With --acme-zone it orders wildcards over DNS-01,
// writing the zone through its provider and asking its servers over
// --dns-resolver (ADR 0045). Its loop is the leader's.
func (i *Instance) newACME(sealer *seal.Sealer) (*acme.Issuer, error) {
	var sl acme.Sealer
	if sealer != nil {
		sl = sealer
	}
	zone, err := acmeZone(i.cfg)
	if err != nil {
		return nil, err
	}
	return acme.New(acme.Config{Directory: i.cfg.ACMEDirectory, Email: i.cfg.ACMEEmail, Profile: i.cfg.ACMEProfile, UserAgent: "ferund/" + i.version,
		ACMEZone: i.cfg.ACMEZone, Log: i.log}, acme.Deps{Store: i.Store, Sealer: sl, Elector: i.Elector, Refresher: domainsRefresher(i),
		Zone: zone, ZoneServers: acme.NewNetZoneServers(i.cfg.DNSResolver, 0)})
}

// acmeZone is the writer of the ACME zone, --acme-zone-provider's; nil
// without --acme-zone.
func acmeZone(cfg config.Config) (acme.Zone, error) {
	if cfg.ACMEZone == "" {
		return nil, nil
	}
	switch cfg.ACMEZoneProvider {
	case config.ACMEZoneRoute53:
		z, err := route53.New(cfg.ACMEZoneRoute53ID, awsauth.NewSource())
		if err != nil {
			return nil, fmt.Errorf("--acme-zone: %w", err)
		}
		return z, nil
	}
	return nil, fmt.Errorf("--acme-zone-provider %q is not known", cfg.ACMEZoneProvider)
}

// domainsRefresher is what ACME calls after each order: the domains
// core's refresh of a domain's state and reason.
func domainsRefresher(i *Instance) acme.Refresher {
	if i.Domains == nil {
		return acme.NoRefresh{}
	}
	return i.Domains
}
