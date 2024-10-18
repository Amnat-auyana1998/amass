// Copyright © by Jeff Foley 2017-2024. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package scrape

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caffix/stringset"
	"github.com/owasp-amass/amass/v4/engine/plugins/support"
	et "github.com/owasp-amass/amass/v4/engine/types"
	"github.com/owasp-amass/amass/v4/utils/net/http"
	dbt "github.com/owasp-amass/asset-db/types"
	oam "github.com/owasp-amass/open-asset-model"
	"github.com/owasp-amass/open-asset-model/domain"
	"github.com/owasp-amass/open-asset-model/source"
	"go.uber.org/ratelimit"
)

type siteDossier struct {
	name   string
	fmtstr string
	log    *slog.Logger
	rlimit ratelimit.Limiter
	source *source.Source
}

func NewSiteDossier() et.Plugin {
	return &siteDossier{
		name:   "SiteDossier",
		fmtstr: "http://www.sitedossier.com/parentdomain/%s/%d",
		rlimit: ratelimit.New(4, ratelimit.WithoutSlack),
		source: &source.Source{
			Name:       "SiteDossier",
			Confidence: 60,
		},
	}
}

func (sd *siteDossier) Name() string {
	return sd.name
}

func (sd *siteDossier) Start(r et.Registry) error {
	sd.log = r.Log().WithGroup("plugin").With("name", sd.name)

	if err := r.RegisterHandler(&et.Handler{
		Plugin:       sd,
		Name:         sd.name + "-Handler",
		Priority:     7,
		MaxInstances: 10,
		Transforms:   []string{string(oam.FQDN)},
		EventType:    oam.FQDN,
		Callback:     sd.check,
	}); err != nil {
		return err
	}

	sd.log.Info("Plugin started")
	return nil
}

func (sd *siteDossier) Stop() {
	sd.log.Info("Plugin stopped")
}

func (sd *siteDossier) check(e *et.Event) error {
	fqdn, ok := e.Asset.Asset.(*domain.FQDN)
	if !ok {
		return errors.New("failed to extract the FQDN asset")
	}

	if a, conf := e.Session.Scope().IsAssetInScope(fqdn, 0); conf == 0 || a == nil {
		return nil
	} else if f, ok := a.(*domain.FQDN); !ok || f == nil || !strings.EqualFold(fqdn.Name, f.Name) {
		return nil
	}

	src := support.GetSource(e.Session, sd.source)
	if src == nil {
		return errors.New("failed to obtain the plugin source information")
	}

	since, err := support.TTLStartTime(e.Session.Config(), string(oam.FQDN), string(oam.FQDN), sd.name)
	if err != nil {
		return err
	}

	var names []*dbt.Asset
	if support.AssetMonitoredWithinTTL(e.Session, e.Asset, src, since) {
		names = append(names, sd.lookup(e, fqdn.Name, src, since)...)
	} else {
		names = append(names, sd.query(e, fqdn.Name, src)...)
		support.MarkAssetMonitored(e.Session, e.Asset, src)
	}

	if len(names) > 0 {
		sd.process(e, names, src)
	}
	return nil
}

func (sd *siteDossier) lookup(e *et.Event, name string, src *dbt.Asset, since time.Time) []*dbt.Asset {
	return support.SourceToAssetsWithinTTL(e.Session, name, string(oam.FQDN), src, since)
}

func (sd *siteDossier) query(e *et.Event, name string, src *dbt.Asset) []*dbt.Asset {
	subs := stringset.New()
	defer subs.Close()

	for i := 1; i < 20; i++ {
		sd.rlimit.Take()
		resp, err := http.RequestWebPage(context.TODO(), &http.Request{URL: fmt.Sprintf(sd.fmtstr, name, i)})
		if err != nil || resp.Body == "" {
			break
		}

		for _, n := range support.ScrapeSubdomainNames(resp.Body) {
			nstr := strings.ToLower(strings.TrimSpace(n))
			// if the subdomain is not in scope, skip it
			if _, conf := e.Session.Scope().IsAssetInScope(&domain.FQDN{Name: nstr}, 0); conf > 0 {
				subs.Insert(nstr)
			}
		}
	}

	return sd.store(e, subs.Slice(), src)
}

func (sd *siteDossier) store(e *et.Event, names []string, src *dbt.Asset) []*dbt.Asset {
	return support.StoreFQDNsWithSource(e.Session, names, src, sd.name, sd.name+"-Handler")
}

func (sd *siteDossier) process(e *et.Event, assets []*dbt.Asset, src *dbt.Asset) {
	support.ProcessFQDNsWithSource(e, assets, src)
}