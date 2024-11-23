// Copyright © by Jeff Foley 2017-2024. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/owasp-amass/amass/v4/engine/plugins/support"
	et "github.com/owasp-amass/amass/v4/engine/types"
	"github.com/owasp-amass/amass/v4/utils/net/dns"
	"github.com/owasp-amass/amass/v4/utils/net/http"
	dbt "github.com/owasp-amass/asset-db/types"
	oam "github.com/owasp-amass/open-asset-model"
	"github.com/owasp-amass/open-asset-model/domain"
	"go.uber.org/ratelimit"
)

type securityTrails struct {
	name   string
	log    *slog.Logger
	rlimit ratelimit.Limiter
	source *et.Source
}

func NewSecurityTrails() et.Plugin {
	return &securityTrails{
		name:   "SecurityTrails",
		rlimit: ratelimit.New(2, ratelimit.WithoutSlack),
		source: &et.Source{
			Name:       "SecurityTrails",
			Confidence: 80,
		},
	}
}

func (st *securityTrails) Name() string {
	return st.name
}

func (st *securityTrails) Start(r et.Registry) error {
	st.log = r.Log().WithGroup("plugin").With("name", st.name)

	if err := r.RegisterHandler(&et.Handler{
		Plugin:       st,
		Name:         st.name + "-Handler",
		Priority:     6,
		MaxInstances: 10,
		Transforms:   []string{string(oam.FQDN)},
		EventType:    oam.FQDN,
		Callback:     st.check,
	}); err != nil {
		return err
	}

	st.log.Info("Plugin started")
	return nil
}

func (st *securityTrails) Stop() {
	st.log.Info("Plugin stopped")
}

func (st *securityTrails) check(e *et.Event) error {
	fqdn, ok := e.Asset.Asset.(*domain.FQDN)
	if !ok {
		return errors.New("failed to extract the FQDN asset")
	}

	ds := e.Session.Config().GetDataSourceConfig(st.name)
	if ds == nil || len(ds.Creds) == 0 {
		return nil
	}

	var keys []string
	for _, cr := range ds.Creds {
		if cr != nil && cr.Apikey != "" {
			keys = append(keys, cr.Apikey)
		}
	}

	if a, conf := e.Session.Scope().IsAssetInScope(fqdn, 0); conf == 0 || a == nil {
		return nil
	} else if f, ok := a.(*domain.FQDN); !ok || f == nil || !strings.EqualFold(fqdn.Name, f.Name) {
		return nil
	}

	src := support.GetSource(e.Session, st.source)
	if src == nil {
		return errors.New("failed to obtain the plugin source information")
	}

	since, err := support.TTLStartTime(e.Session.Config(), string(oam.FQDN), string(oam.FQDN), st.name)
	if err != nil {
		return err
	}

	var names []*dbt.Entity
	if support.AssetMonitoredWithinTTL(e.Session, e.Asset, src, since) {
		names = append(names, st.lookup(e, fqdn.Name, src, since)...)
	} else {
		names = append(names, st.query(e, fqdn.Name, src, keys)...)
		support.MarkAssetMonitored(e.Session, e.Asset, src)
	}

	if len(names) > 0 {
		st.process(e, names, src)
	}
	return nil
}

func (st *securityTrails) lookup(e *et.Event, name string, src *et.Source, since time.Time) []*dbt.Entity {
	return support.SourceToAssetsWithinTTL(e.Session, name, string(oam.FQDN), src, since)
}

func (st *securityTrails) query(e *et.Event, name string, src *et.Source, keys []string) []*dbt.Entity {
	var names []string

	for _, key := range keys {
		st.rlimit.Take()
		resp, err := http.RequestWebPage(context.TODO(), &http.Request{
			URL:    "https://api.securitytrails.com/v1/domain/" + name + "/subdomains",
			Header: http.Header{"APIKEY": []string{key}},
		})
		if err != nil || resp.Body == "" {
			continue
		}

		var result struct {
			Subdomains []string `json:"subdomains"`
		}
		if err := json.Unmarshal([]byte(resp.Body), &result); err != nil {
			continue
		}

		for _, sub := range result.Subdomains {
			nstr := strings.ToLower(strings.TrimSpace(dns.RemoveAsteriskLabel(sub + "." + name)))
			// if the subdomain is not in scope, skip it
			if _, conf := e.Session.Scope().IsAssetInScope(&domain.FQDN{Name: nstr}, 0); conf > 0 {
				names = append(names, nstr)
			}
		}
		break
	}

	return st.store(e, names, src)
}

func (st *securityTrails) store(e *et.Event, names []string, src *et.Source) []*dbt.Entity {
	return support.StoreFQDNsWithSource(e.Session, names, src, st.name, st.name+"-Handler")
}

func (st *securityTrails) process(e *et.Event, assets []*dbt.Entity, src *et.Source) {
	support.ProcessFQDNsWithSource(e, assets, src)
}
