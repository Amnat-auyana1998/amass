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
	"github.com/owasp-amass/open-asset-model/source"
	"go.uber.org/ratelimit"
)

type crtsh struct {
	name   string
	log    *slog.Logger
	rlimit ratelimit.Limiter
	source *source.Source
}

func NewCrtsh() et.Plugin {
	return &crtsh{
		name:   "crt.sh",
		rlimit: ratelimit.New(2, ratelimit.WithoutSlack),
		source: &source.Source{
			Name:       "HackerTarget",
			Confidence: 100,
		},
	}
}

func (c *crtsh) Name() string {
	return c.name
}

func (c *crtsh) Start(r et.Registry) error {
	c.log = r.Log().WithGroup("plugin").With("name", c.name)

	if err := r.RegisterHandler(&et.Handler{
		Plugin:       c,
		Name:         c.name + "-Handler",
		Priority:     5,
		MaxInstances: 10,
		Transforms:   []string{string(oam.FQDN)},
		EventType:    oam.FQDN,
		Callback:     c.check,
	}); err != nil {
		return err
	}

	c.log.Info("Plugin started")
	return nil
}

func (c *crtsh) Stop() {
	c.log.Info("Plugin stopped")
}

func (c *crtsh) check(e *et.Event) error {
	fqdn, ok := e.Asset.Asset.(*domain.FQDN)
	if !ok {
		return errors.New("failed to extract the FQDN asset")
	}

	if a, conf := e.Session.Scope().IsAssetInScope(fqdn, 0); conf == 0 || a == nil {
		return nil
	} else if f, ok := a.(*domain.FQDN); !ok || f == nil || !strings.EqualFold(fqdn.Name, f.Name) {
		return nil
	}

	src := support.GetSource(e.Session, c.source)
	if src == nil {
		return errors.New("failed to obtain the plugin source information")
	}

	since, err := support.TTLStartTime(e.Session.Config(), string(oam.FQDN), string(oam.FQDN), c.name)
	if err != nil {
		return err
	}

	var names []*dbt.Asset
	if support.AssetMonitoredWithinTTL(e.Session, e.Asset, src, since) {
		names = append(names, c.lookup(e, fqdn.Name, src, since)...)
	} else {
		names = append(names, c.query(e, fqdn.Name, src)...)
		support.MarkAssetMonitored(e.Session, e.Asset, src)
	}

	if len(names) > 0 {
		c.process(e, names, src)
	}
	return nil
}

func (c *crtsh) lookup(e *et.Event, name string, src *dbt.Asset, since time.Time) []*dbt.Asset {
	return support.SourceToAssetsWithinTTL(e.Session, name, string(oam.FQDN), src, since)
}

func (c *crtsh) query(e *et.Event, name string, src *dbt.Asset) []*dbt.Asset {
	c.rlimit.Take()
	resp, err := http.RequestWebPage(context.TODO(), &http.Request{
		URL: "https://crt.sh/?CN=" + name + "&output=json&exclude=expired",
	})
	if err != nil {
		return []*dbt.Asset{}
	}

	var result struct {
		Certs []struct {
			Names string `json:"name_value"`
		} `json:"certs"`
	}
	if err := json.Unmarshal([]byte("{\"certs\":"+resp.Body+"}"), &result); err != nil {
		return []*dbt.Asset{}
	}

	var names []string
	for _, cert := range result.Certs {
		for _, n := range strings.Split(cert.Names, "\n") {
			nstr := strings.ToLower(strings.TrimSpace(dns.RemoveAsteriskLabel(n)))
			// if the subdomain is not in scope, skip it
			if _, conf := e.Session.Scope().IsAssetInScope(&domain.FQDN{Name: nstr}, 0); conf > 0 {
				names = append(names, nstr)
			}
		}
	}

	return c.store(e, names, src)
}

func (c *crtsh) store(e *et.Event, names []string, src *dbt.Asset) []*dbt.Asset {
	return support.StoreFQDNsWithSource(e.Session, names, src, c.name, c.name+"-Handler")
}

func (c *crtsh) process(e *et.Event, assets []*dbt.Asset, src *dbt.Asset) {
	support.ProcessFQDNsWithSource(e, assets, src)
}
