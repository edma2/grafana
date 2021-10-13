package api

import (
	"errors"
	"strconv"

	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/tsdb/grafanads"
	"github.com/grafana/grafana/pkg/util"
)

func (hs *HTTPServer) getFSDataSources(c *models.ReqContext, enabledPlugins EnabledPlugins) (map[string]interface{}, error) {
	orgDataSources := make([]*models.DataSource, 0)

	if c.OrgId != 0 {
		query := models.GetDataSourcesQuery{OrgId: c.OrgId, DataSourceLimit: hs.Cfg.DataSourceLimit}
		err := bus.Dispatch(&query)

		if err != nil {
			return nil, err
		}

		dsFilterQuery := models.DatasourcesPermissionFilterQuery{
			User:        c.SignedInUser,
			Datasources: query.Result,
		}

		if err := bus.Dispatch(&dsFilterQuery); err != nil {
			if !errors.Is(err, bus.ErrHandlerNotFound) {
				return nil, err
			}

			orgDataSources = query.Result
		} else {
			orgDataSources = dsFilterQuery.Result
		}
	}

	dataSources := make(map[string]interface{})

	for _, ds := range orgDataSources {
		url := ds.Url

		if ds.Access == models.DS_ACCESS_PROXY {
			url = "/api/datasources/proxy/" + strconv.FormatInt(ds.Id, 10)
		}

		dsMap := map[string]interface{}{
			"id":        ds.Id,
			"uid":       ds.Uid,
			"type":      ds.Type,
			"name":      ds.Name,
			"url":       url,
			"isDefault": ds.IsDefault,
			"access":    ds.Access,
		}

		meta, exists := enabledPlugins.Get(plugins.DataSource, ds.Type)
		if !exists {
			log.Errorf(3, "Could not find plugin definition for data source: %v", ds.Type)
			continue
		}
		dsMap["preload"] = meta.Preload
		dsMap["module"] = meta.Module
		dsMap["meta"] = &plugins.PluginDTO{
			JSONData:  meta.JSONData,
			Signature: meta.Signature,
			Module:    meta.Module,
			BaseURL:   meta.BaseURL,
		}

		jsonData := ds.JsonData
		if jsonData == nil {
			jsonData = simplejson.New()
		}

		dsMap["jsonData"] = jsonData

		if ds.Access == models.DS_ACCESS_DIRECT {
			if ds.BasicAuth {
				dsMap["basicAuth"] = util.GetBasicAuthHeader(
					ds.BasicAuthUser,
					hs.DataSourcesService.DecryptedBasicAuthPassword(ds),
				)
			}
			if ds.WithCredentials {
				dsMap["withCredentials"] = ds.WithCredentials
			}

			if ds.Type == models.DS_INFLUXDB_08 {
				dsMap["username"] = ds.User
				dsMap["password"] = hs.DataSourcesService.DecryptedPassword(ds)
				dsMap["url"] = url + "/db/" + ds.Database
			}

			if ds.Type == models.DS_INFLUXDB {
				dsMap["username"] = ds.User
				dsMap["password"] = hs.DataSourcesService.DecryptedPassword(ds)
				dsMap["url"] = url
			}
		}

		if (ds.Type == models.DS_INFLUXDB) || (ds.Type == models.DS_ES) {
			dsMap["database"] = ds.Database
		}

		if ds.Type == models.DS_PROMETHEUS {
			// add unproxied server URL for link to Prometheus web UI
			jsonData.Set("directUrl", ds.Url)
		}

		dataSources[ds.Name] = dsMap
	}

	// add data sources that are built in (meaning they are not added via data sources page, nor have any entry in
	// the datasource table)
	for _, ds := range hs.pluginStore.Plugins(plugins.DataSource) {
		if ds.BuiltIn {
			info := map[string]interface{}{
				"type": ds.Type,
				"name": ds.Name,
				"meta": &plugins.PluginDTO{
					JSONData:  ds.JSONData,
					Signature: ds.Signature,
					Module:    ds.Module,
					BaseURL:   ds.BaseURL,
				},
			}
			if ds.Name == grafanads.DatasourceName {
				info["id"] = grafanads.DatasourceID
				info["uid"] = grafanads.DatasourceUID
			}
			dataSources[ds.Name] = info
		}
	}

	return dataSources, nil
}

// getFrontendSettingsMap returns a json object with all the settings needed for front end initialisation.
func (hs *HTTPServer) getFrontendSettingsMap(c *models.ReqContext) (map[string]interface{}, error) {
	enabledPlugins, err := hs.enabledPlugins(c.OrgId)
	if err != nil {
		return nil, err
	}

	pluginsToPreload := []string{}
	for _, app := range enabledPlugins[plugins.App] {
		if app.Preload {
			pluginsToPreload = append(pluginsToPreload, app.Module)
		}
	}

	dataSources, err := hs.getFSDataSources(c, enabledPlugins)
	if err != nil {
		return nil, err
	}

	defaultDS := "-- Grafana --"
	for n, ds := range dataSources {
		dsM := ds.(map[string]interface{})
		if isDefault, _ := dsM["isDefault"].(bool); isDefault {
			defaultDS = n
		}

		module, _ := dsM["module"].(string)
		if preload, _ := dsM["preload"].(bool); preload && module != "" {
			pluginsToPreload = append(pluginsToPreload, module)
		}
	}

	panels := map[string]interface{}{}
	for _, panel := range enabledPlugins[plugins.Panel] {
		if panel.State == plugins.AlphaRelease && !hs.Cfg.PluginsEnableAlpha {
			continue
		}

		if panel.Preload {
			pluginsToPreload = append(pluginsToPreload, panel.Module)
		}

		panels[panel.ID] = map[string]interface{}{
			"id":            panel.ID,
			"module":        panel.Module,
			"baseUrl":       panel.BaseURL,
			"name":          panel.Name,
			"info":          panel.Info,
			"hideFromList":  panel.HideFromList,
			"sort":          getPanelSort(panel.ID),
			"skipDataQuery": panel.SkipDataQuery,
			"state":         panel.State,
			"signature":     panel.Signature,
		}
	}

	hideVersion := hs.Cfg.AnonymousHideVersion && !c.IsSignedIn
	version := setting.BuildVersion
	commit := setting.BuildCommit
	buildstamp := setting.BuildStamp

	if hideVersion {
		version = ""
		commit = ""
		buildstamp = 0
	}

	hasAccess := accesscontrol.HasAccess(hs.AccessControl, c)

	jsonObj := map[string]interface{}{
		"defaultDatasource":                   defaultDS,
		"datasources":                         dataSources,
		"minRefreshInterval":                  setting.MinRefreshInterval,
		"panels":                              panels,
		"appUrl":                              hs.Cfg.AppURL,
		"appSubUrl":                           hs.Cfg.AppSubURL,
		"allowOrgCreate":                      (setting.AllowUserOrgCreate && c.IsSignedIn) || c.IsGrafanaAdmin,
		"authProxyEnabled":                    setting.AuthProxyEnabled,
		"ldapEnabled":                         hs.Cfg.LDAPEnabled,
		"alertingEnabled":                     setting.AlertingEnabled,
		"alertingErrorOrTimeout":              setting.AlertingErrorOrTimeout,
		"alertingNoDataOrNullValues":          setting.AlertingNoDataOrNullValues,
		"alertingMinInterval":                 setting.AlertingMinInterval,
		"liveEnabled":                         hs.Cfg.LiveMaxConnections != 0,
		"autoAssignOrg":                       setting.AutoAssignOrg,
		"verifyEmailEnabled":                  setting.VerifyEmailEnabled,
		"sigV4AuthEnabled":                    setting.SigV4AuthEnabled,
		"exploreEnabled":                      setting.ExploreEnabled,
		"googleAnalyticsId":                   setting.GoogleAnalyticsId,
		"rudderstackWriteKey":                 setting.RudderstackWriteKey,
		"rudderstackDataPlaneUrl":             setting.RudderstackDataPlaneUrl,
		"applicationInsightsConnectionString": hs.Cfg.ApplicationInsightsConnectionString,
		"applicationInsightsEndpointUrl":      hs.Cfg.ApplicationInsightsEndpointUrl,
		"disableLoginForm":                    setting.DisableLoginForm,
		"disableUserSignUp":                   !setting.AllowUserSignUp,
		"loginHint":                           setting.LoginHint,
		"passwordHint":                        setting.PasswordHint,
		"externalUserMngInfo":                 setting.ExternalUserMngInfo,
		"externalUserMngLinkUrl":              setting.ExternalUserMngLinkUrl,
		"externalUserMngLinkName":             setting.ExternalUserMngLinkName,
		"viewersCanEdit":                      setting.ViewersCanEdit,
		"editorsCanAdmin":                     hs.Cfg.EditorsCanAdmin,
		"disableSanitizeHtml":                 hs.Cfg.DisableSanitizeHtml,
		"pluginsToPreload":                    pluginsToPreload,
		"buildInfo": map[string]interface{}{
			"hideVersion":   hideVersion,
			"version":       version,
			"commit":        commit,
			"buildstamp":    buildstamp,
			"edition":       hs.License.Edition(),
			"latestVersion": hs.updateChecker.LatestVersion,
			"hasUpdate":     hs.updateChecker.HasUpdate,
			"env":           setting.Env,
			"isEnterprise":  hs.License.HasValidLicense(),
		},
		"licenseInfo": map[string]interface{}{
			"hasLicense":      hs.License.HasLicense(),
			"hasValidLicense": hs.License.HasValidLicense(),
			"expiry":          hs.License.Expiry(),
			"stateInfo":       hs.License.StateInfo(),
			"licenseUrl":      hs.License.LicenseURL(hasAccess(accesscontrol.ReqGrafanaAdmin, accesscontrol.LicensingPageReaderAccess)),
			"edition":         hs.License.Edition(),
		},
		"featureToggles":                   hs.Cfg.FeatureToggles,
		"rendererAvailable":                hs.RenderService.IsAvailable(),
		"rendererVersion":                  hs.RenderService.Version(),
		"http2Enabled":                     hs.Cfg.Protocol == setting.HTTP2Scheme,
		"sentry":                           hs.Cfg.Sentry,
		"pluginCatalogURL":                 hs.Cfg.PluginCatalogURL,
		"pluginAdminEnabled":               hs.Cfg.PluginAdminEnabled,
		"pluginAdminExternalManageEnabled": hs.Cfg.PluginAdminEnabled && hs.Cfg.PluginAdminExternalManageEnabled,
		"expressionsEnabled":               hs.Cfg.ExpressionsEnabled,
		"awsAllowedAuthProviders":          hs.Cfg.AWSAllowedAuthProviders,
		"awsAssumeRoleEnabled":             hs.Cfg.AWSAssumeRoleEnabled,
		"azure": map[string]interface{}{
			"cloud":                  hs.Cfg.Azure.Cloud,
			"managedIdentityEnabled": hs.Cfg.Azure.ManagedIdentityEnabled,
		},
		"caching": map[string]bool{
			"enabled": hs.Cfg.SectionWithEnvOverrides("caching").Key("enabled").MustBool(true),
		},
		"unifiedAlertingEnabled": hs.Cfg.UnifiedAlerting.Enabled,
	}

	if hs.Cfg.GeomapDefaultBaseLayerConfig != nil {
		jsonObj["geomapDefaultBaseLayerConfig"] = hs.Cfg.GeomapDefaultBaseLayerConfig
	}
	if !hs.Cfg.GeomapEnableCustomBaseLayers {
		jsonObj["geomapDisableCustomBaseLayer"] = true
	}

	return jsonObj, nil
}

func getPanelSort(id string) int {
	sort := 100
	switch id {
	case "timeseries":
		sort = 1
	case "barchart":
		sort = 2
	case "stat":
		sort = 3
	case "gauge":
		sort = 4
	case "bargauge":
		sort = 5
	case "table":
		sort = 6
	case "singlestat":
		sort = 7
	case "piechart":
		sort = 8
	case "state-timeline":
		sort = 9
	case "heatmap":
		sort = 10
	case "status-history":
		sort = 11
	case "histogram":
		sort = 12
	case "graph":
		sort = 13
	case "text":
		sort = 14
	case "alertlist":
		sort = 15
	case "dashlist":
		sort = 16
	case "news":
		sort = 17
	}
	return sort
}

func (hs *HTTPServer) GetFrontendSettings(c *models.ReqContext) {
	settings, err := hs.getFrontendSettingsMap(c)
	if err != nil {
		c.JsonApiErr(400, "Failed to get frontend settings", err)
		return
	}

	c.JSON(200, settings)
}

// EnabledPlugins represents a mapping of plugin types to plugin IDs to plugins
type EnabledPlugins map[plugins.Type]map[string]*plugins.Plugin

func (ep EnabledPlugins) Get(pluginType plugins.Type, pluginID string) (*plugins.Plugin, bool) {
	if _, exists := ep[pluginType][pluginID]; exists {
		return ep[pluginType][pluginID], true
	}

	return nil, false
}

func (hs *HTTPServer) enabledPlugins(orgID int64) (EnabledPlugins, error) {
	ep := make(EnabledPlugins)

	pluginSettingMap, err := hs.pluginSettings(orgID)
	if err != nil {
		return ep, err
	}

	apps := make(map[string]*plugins.Plugin)
	for _, app := range hs.pluginStore.Plugins(plugins.App) {
		if b, ok := pluginSettingMap[app.ID]; ok {
			app.Pinned = b.Pinned
			apps[app.ID] = app
		}
	}
	ep[plugins.App] = apps

	dataSources := make(map[string]*plugins.Plugin)
	for _, ds := range hs.pluginStore.Plugins(plugins.DataSource) {
		if _, exists := pluginSettingMap[ds.ID]; exists {
			dataSources[ds.ID] = ds
		}
	}
	ep[plugins.DataSource] = dataSources

	panels := make(map[string]*plugins.Plugin)
	for _, p := range hs.pluginStore.Plugins(plugins.Panel) {
		if _, exists := pluginSettingMap[p.ID]; exists {
			panels[p.ID] = p
		}
	}
	ep[plugins.Panel] = panels

	return ep, nil
}

func (hs *HTTPServer) pluginSettings(orgID int64) (map[string]*models.PluginSettingInfoDTO, error) {
	pluginSettings, err := hs.SQLStore.GetPluginSettings(orgID)
	if err != nil {
		return nil, err
	}

	pluginMap := make(map[string]*models.PluginSettingInfoDTO)
	for _, plug := range pluginSettings {
		pluginMap[plug.PluginId] = plug
	}

	for _, pluginDef := range hs.pluginStore.Plugins() {
		// ignore entries that exists
		if _, ok := pluginMap[pluginDef.ID]; ok {
			continue
		}

		// enabled by default
		opt := &models.PluginSettingInfoDTO{
			PluginId: pluginDef.ID,
			OrgId:    orgID,
			Enabled:  true,
		}

		// apps are disabled by default unless autoEnabled: true
		if p := hs.pluginStore.Plugin(pluginDef.ID); p != nil && p.IsApp() {
			opt.Enabled = p.AutoEnabled
			opt.Pinned = p.AutoEnabled
		}

		// if it's included in app check app settings
		if pluginDef.IncludedInAppID != "" {
			// app components are by default disabled
			opt.Enabled = false

			if appSettings, ok := pluginMap[pluginDef.IncludedInAppID]; ok {
				opt.Enabled = appSettings.Enabled
			}
		}

		pluginMap[pluginDef.ID] = opt
	}

	return pluginMap, nil
}
