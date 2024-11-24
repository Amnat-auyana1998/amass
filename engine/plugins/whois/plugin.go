// Copyright © by Jeff Foley 2017-2024. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package whois

import (
	"log/slog"

	et "github.com/owasp-amass/amass/v4/engine/types"
	oam "github.com/owasp-amass/open-asset-model"
	"go.uber.org/ratelimit"
)

type whois struct {
	name   string
	log    *slog.Logger
	rlimit ratelimit.Limiter
	fqdn   *fqdnLookup
	domrec *domrec
	source *et.Source
}

func NewWHOIS() et.Plugin {
	return &whois{
		name:   "WHOIS",
		rlimit: ratelimit.New(10, ratelimit.WithoutSlack),
		source: &et.Source{
			Name:       "WHOIS",
			Confidence: 100,
		},
	}
}

func (w *whois) Name() string {
	return w.name
}

func (w *whois) Start(r et.Registry) error {
	w.log = r.Log().WithGroup("plugin").With("name", w.name)

	w.fqdn = &fqdnLookup{
		name:   w.name + "-FQDN-Handler",
		plugin: w,
	}
	if err := r.RegisterHandler(&et.Handler{
		Plugin:     w,
		Name:       w.fqdn.name,
		Priority:   3,
		Transforms: []string{string(oam.DomainRecord)},
		EventType:  oam.FQDN,
		Callback:   w.fqdn.check,
	}); err != nil {
		return err
	}

	w.domrec = &domrec{
		name:   w.name + "-Domain-Record-Handler",
		plugin: w,
		transforms: []string{
			string(oam.FQDN),
			string(oam.URL),
			string(oam.ContactRecord),
			string(oam.Person),
			string(oam.Organization),
			string(oam.Location),
			string(oam.EmailAddress),
			string(oam.Phone),
		},
	}
	if err := r.RegisterHandler(&et.Handler{
		Plugin:     w,
		Name:       w.domrec.name,
		Transforms: w.domrec.transforms,
		EventType:  oam.DomainRecord,
		Callback:   w.domrec.check,
	}); err != nil {
		return err
	}

	w.log.Info("Plugin started")
	return nil
}

func (w *whois) Stop() {
	w.log.Info("Plugin stopped")
}
