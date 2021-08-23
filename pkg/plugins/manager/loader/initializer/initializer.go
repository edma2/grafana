package initializer

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/grafana/grafana-aws-sdk/pkg/awsds"

	"github.com/gosimple/slug"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/metrics"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/plugins/backendplugin"
	"github.com/grafana/grafana/pkg/plugins/backendplugin/grpcplugin"
	"github.com/grafana/grafana/pkg/plugins/backendplugin/pluginextensionv2"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
)

var logger = log.New("plugin.initializer")

type Initializer struct {
	cfg     *setting.Cfg
	license models.Licensing
}

func New(cfg *setting.Cfg, license models.Licensing) Initializer {
	return Initializer{
		cfg:     cfg,
		license: license,
	}
}

func (i *Initializer) Initialize(p *plugins.PluginV2) error {
	if len(p.Dependencies.Plugins) == 0 {
		p.Dependencies.Plugins = []plugins.PluginDependencyItem{}
	}

	if p.Dependencies.GrafanaVersion == "" {
		p.Dependencies.GrafanaVersion = "*"
	}

	for _, include := range p.Includes {
		if include.Role == "" {
			include.Role = models.ROLE_VIEWER
		}
	}

	i.handleModuleDefaults(p)

	p.Info.Logos.Small = getPluginLogoUrl(p.Type, p.Info.Logos.Small, p.BaseURL)
	p.Info.Logos.Large = getPluginLogoUrl(p.Type, p.Info.Logos.Large, p.BaseURL)

	for i := 0; i < len(p.Info.Screenshots); i++ {
		p.Info.Screenshots[i].Path = evalRelativePluginUrlPath(p.Info.Screenshots[i].Path, p.BaseURL, p.Type)
	}

	if p.IsApp() {
		for _, child := range p.Children {
			i.setPathsBasedOnApp(p, child)
		}

		// slugify pages
		for _, include := range p.Includes {
			if include.Slug == "" {
				include.Slug = slug.Make(include.Name)
			}
			if include.Type == "page" && include.DefaultNav {
				p.DefaultNavURL = i.cfg.AppSubURL + "/plugins/" + p.ID + "/page/" + include.Slug
			}
			if include.Type == "dashboard" && include.DefaultNav {
				p.DefaultNavURL = i.cfg.AppSubURL + "/dashboard/db/" + include.Slug
			}
		}
	}

	if !p.IsCorePlugin() {
		logger.Info(fmt.Sprintf("Successfully added %s plugin", p.Class), "pluginID", p.ID)
	}

	pluginLog := logger.New("pluginID", p.ID)
	p.SetLogger(pluginLog)

	if p.Backend {
		var backendFactory backendplugin.PluginFactoryFunc
		if p.IsRenderer() {
			cmd := plugins.ComposeRendererStartCommand()
			backendFactory = grpcplugin.NewRendererPlugin(p.ID, filepath.Join(p.PluginDir, cmd),
				func(pluginID string, renderer pluginextensionv2.RendererPlugin, logger log.Logger) error {
					p.Renderer = renderer
					return nil
				},
			)
		} else {
			cmd := plugins.ComposePluginStartCommand(p.Executable)
			backendFactory = grpcplugin.NewBackendPlugin(p.ID, filepath.Join(p.PluginDir, cmd))
		}

		env := i.getPluginEnvVars(p)

		if backendClient, err := backendFactory(p.ID, pluginLog, env); err != nil {
			return err
		} else {
			p.RegisterClient(backendClient)
		}
	}

	return nil
}

func (i *Initializer) InitializeWithFactory(p *plugins.PluginV2, factory backendplugin.PluginFactoryFunc) error {
	err := i.Initialize(p)
	if err != nil {
		return err
	}

	if p.IsCorePlugin() {
		if factory != nil {
			var err error

			p.Client, err = factory(p.ID, log.New("pluginID", p.ID), []string{})
			if err != nil {
				return err
			}
		} else {
			logger.Warn("Could not initialize core plugin process", "pluginID", p.ID)
		}
	}

	return nil
}

func (i *Initializer) handleModuleDefaults(p *plugins.PluginV2) {
	if p.IsExternalPlugin() {
		metrics.SetPluginBuildInformation(p.ID, string(p.Type), p.Info.Version, string(p.Signature))

		p.Module = path.Join("plugins", p.ID, "module")
		p.BaseURL = path.Join("public/plugins", p.ID)
		return
	}

	// Previously there was an assumption that the plugin directory
	// should be public/app/plugins/<plugin type>/<plugin id>
	// However this can be an issue if the plugin directory should be renamed to something else
	currentDir := filepath.Base(p.PluginDir)
	// use path package for the following statements
	// because these are not file paths
	p.Module = path.Join("app/plugins", string(p.Type), currentDir, "module")
	p.BaseURL = path.Join("public/app/plugins", string(p.Type), currentDir)
}

func (i *Initializer) setPathsBasedOnApp(parent *plugins.PluginV2, child *plugins.PluginV2) {
	appSubPath := strings.ReplaceAll(strings.Replace(child.PluginDir, parent.PluginDir, "", 1), "\\", "/")
	child.IncludedInAppID = parent.ID
	child.BaseURL = parent.BaseURL

	if parent.IsExternalPlugin() {
		child.Module = util.JoinURLFragments("plugins/"+parent.ID, appSubPath) + "/module"
	} else {
		child.Module = util.JoinURLFragments("app/plugins/app/"+parent.ID, appSubPath) + "/module"
	}
}

func getPluginLogoUrl(pluginType plugins.PluginType, path, baseUrl string) string {
	if path == "" {
		return defaultLogoPath(pluginType)
	}

	return evalRelativePluginUrlPath(path, baseUrl, pluginType)
}

func defaultLogoPath(pluginType plugins.PluginType) string {
	return "public/img/icn-" + string(pluginType) + ".svg"
}

func evalRelativePluginUrlPath(pathStr, baseUrl string, pluginType plugins.PluginType) string {
	if pathStr == "" {
		return ""
	}

	u, _ := url.Parse(pathStr)
	if u.IsAbs() {
		return pathStr
	}

	// is set as default or has already been prefixed with base path
	if pathStr == defaultLogoPath(pluginType) || strings.HasPrefix(pathStr, baseUrl) {
		return pathStr
	}

	return path.Join(baseUrl, pathStr)
}

func (i *Initializer) getPluginEnvVars(plugin *plugins.PluginV2) []string {
	hostEnv := []string{
		fmt.Sprintf("GF_VERSION=%s", i.cfg.BuildVersion),
	}

	if i.license != nil && i.license.HasLicense() {
		hostEnv = append(
			hostEnv,
			fmt.Sprintf("GF_EDITION=%s", i.license.Edition()),
			fmt.Sprintf("GF_ENTERPRISE_license_PATH=%s", i.cfg.EnterpriseLicensePath),
		)

		if envProvider, ok := i.license.(models.LicenseEnvironment); ok {
			for k, v := range envProvider.Environment() {
				hostEnv = append(hostEnv, fmt.Sprintf("%s=%s", k, v))
			}
		}
	}

	hostEnv = append(hostEnv, i.getAWSEnvironmentVariables()...)
	hostEnv = append(hostEnv, i.getAzureEnvironmentVariables()...)
	env := getPluginSettings(plugin.ID, i.cfg).ToEnv("GF_PLUGIN", hostEnv)
	return env
}

func (i *Initializer) getAWSEnvironmentVariables() []string {
	var variables []string
	if i.cfg.AWSAssumeRoleEnabled {
		variables = append(variables, awsds.AssumeRoleEnabledEnvVarKeyName+"=true")
	}
	if len(i.cfg.AWSAllowedAuthProviders) > 0 {
		variables = append(variables, awsds.AllowedAuthProvidersEnvVarKeyName+"="+strings.Join(i.cfg.AWSAllowedAuthProviders, ","))
	}

	return variables
}

func (i *Initializer) getAzureEnvironmentVariables() []string {
	var variables []string
	if i.cfg.Azure.Cloud != "" {
		variables = append(variables, "AZURE_CLOUD="+i.cfg.Azure.Cloud)
	}
	if i.cfg.Azure.ManagedIdentityClientId != "" {
		variables = append(variables, "AZURE_MANAGED_IDENTITY_CLIENT_ID="+i.cfg.Azure.ManagedIdentityClientId)
	}
	if i.cfg.Azure.ManagedIdentityEnabled {
		variables = append(variables, "AZURE_MANAGED_IDENTITY_ENABLED=true")
	}

	return variables
}

type pluginSettings map[string]string

func (ps pluginSettings) ToEnv(prefix string, hostEnv []string) []string {
	var env []string
	for k, v := range ps {
		key := fmt.Sprintf("%s_%s", prefix, strings.ToUpper(k))
		if value := os.Getenv(key); value != "" {
			v = value
		}

		env = append(env, fmt.Sprintf("%s=%s", key, v))
	}

	env = append(env, hostEnv...)

	return env
}

func getPluginSettings(plugID string, cfg *setting.Cfg) pluginSettings {
	ps := pluginSettings{}
	for k, v := range cfg.PluginSettings[plugID] {
		if k == "path" || strings.ToLower(k) == "id" {
			continue
		}
		ps[k] = v
	}

	return ps
}