// Package provider contains the Tharsis provider configuration.
package provider

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	svchost "github.com/hashicorp/terraform-svchost"

	tharsis "gitlab.com/infor-cloud/martian-cloud/tharsis/tharsis-sdk-go/pkg"
	"gitlab.com/infor-cloud/martian-cloud/tharsis/tharsis-sdk-go/pkg/auth"
	"gitlab.com/infor-cloud/martian-cloud/tharsis/tharsis-sdk-go/pkg/config"
)

// Ensure provider defined types fully satisfy framework interfaces
var _ provider.Provider = &tharsisProvider{}

const scheme string = "https://"

// New creates a new instance of the Tharsis provider
func New() provider.Provider {
	return &tharsisProvider{
		version: Version,
	}
}

// tharsisProvider satisfies the provider.Provider interface and usually is included
// with all Resource and DataSource implementations.
type tharsisProvider struct {
	// configured is set to true at the end of the Configure method.
	// This can be used in Resource and DataSource implementations to verify
	// that the provider was previously configured.
	configured bool

	// version is set to the provider version on release, "dev" when the
	// provider is built and ran locally, and "test" when running acceptance
	// testing.
	version string

	// client is the Tharsis SDK Client that will be used to make the API calls.
	client *tharsis.Client
}

func (p *tharsisProvider) GetSchema(ctx context.Context) (tfsdk.Schema, diag.Diagnostics) {
	return tfsdk.Schema{
		Attributes: map[string]tfsdk.Attribute{
			"host": {
				Type:                types.StringType,
				Description:         "Host name of the Tharsis API (e.g. https://tharsis.example.com)",
				MarkdownDescription: "This is the hostname for the Tharsis API (e.g. https://tharsis.example.com).",
				Optional:            true,
				Computed:            true,
			},
			"static_token": {
				Type:                types.StringType,
				Description:         "Static token to authenticate with the Tharsis API",
				MarkdownDescription: "A static token to use to authenticate with the Tharsis API.",
				Optional:            true,
				Computed:            true,
			},
			"service_account_path": {
				Type:                types.StringType,
				Description:         "Service account path to use for authenticating with the Tharsis API",
				MarkdownDescription: "A Service account path to use for authenticating with the Tharsis API.",
				Optional:            true,
				Computed:            true,
			},
			"service_account_token": {
				Type:                types.StringType,
				Description:         "Service account token to use for authenticating with the Tharsis API",
				MarkdownDescription: "A Service account token to use for authenticating with the Tharsis API.",
				Optional:            true,
				Computed:            true,
			},
		},
	}, nil
}

// providerData can be used to store data from the Terraform configuration.
type providerData struct {
	Host                types.String `tfsdk:"host"`
	StaticToken         types.String `tfsdk:"static_token"`
	ServiceAccountPath  types.String `tfsdk:"service_account_path"`
	ServiceAccountToken types.String `tfsdk:"service_account_token"`
}

// checkUnknowns validates that no field is unknown during configuration
func (pd *providerData) checkUnknowns() diag.Diagnostics {
	var diags diag.Diagnostics
	if pd.Host.Unknown {
		// Cannot connect to client with an unknown value
		diags = append(diags,
			diag.NewErrorDiagnostic(
				"Unknown host name",
				"Cannot use an unknown value as host",
			),
		)
	}

	if pd.StaticToken.Unknown {
		diags = append(diags,
			diag.NewErrorDiagnostic(
				"Unknown static token",
				"Cannot use an unknown value as static token",
			),
		)
	}

	if pd.ServiceAccountPath.Unknown {
		diags = append(diags,
			diag.NewErrorDiagnostic(
				"Unknown service account path",
				"Cannot use an unknown value as service account path",
			),
		)
	}

	if pd.ServiceAccountToken.Unknown {
		diags = append(diags,
			diag.NewErrorDiagnostic(
				"Unknown service account token",
				"Cannot use an unknown value as service account token",
			),
		)
	}

	return diags
}

func (p *tharsisProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data providerData

	diags := req.Config.Get(ctx, &data)
	if diags.HasError() {
		resp.Diagnostics.Append(diags...)
		return
	}

	// No field in the provider can be unknown
	diags = data.checkUnknowns()
	if diags.HasError() {
		resp.Diagnostics.Append(diags...)
		return
	}

	tClient, err := newTharsisClient(ctx, &data)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error configuring the Tharsis client",
			fmt.Sprintf("Error configuring the Tharsis client, this is an error in the provider.\n%s\n", err),
		)
	}

	p.client = tClient
	p.configured = true

	// Make the Tharsis client available during DataSource and Resource
	// type Configure methods.
	resp.DataSourceData = tClient
	resp.ResourceData = tClient

	tflog.Info(ctx, "Configured Tharsis client", map[string]any{"success": true})
}

func (p *tharsisProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewGroupResource,
		NewManagedIdentityResource,
		NewManagedIdentityAccessRuleResource,
		NewServiceAccountResource,
		NewVariableResource,
		NewWorkspaceResource,
	}
}

func (p *tharsisProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{

		// tharsis_workspace_outputs, no JSON
		func() datasource.DataSource {
			return workspaceOutputsDataSource{
				provider:      *p,
				isJSONEncoded: false,
			}
		},

		// tharsis_workspace_outputs, with JSON
		func() datasource.DataSource {
			return workspaceOutputsDataSource{
				provider:      *p,
				isJSONEncoded: true,
			}
		},
	}
}

func newTharsisClient(_ context.Context, pd *providerData) (*tharsis.Client, error) {
	var (
		host                                    string
		staticToken                             string
		serviceAccountPath, serviceAccountToken string
		optFn                                   []func(*config.LoadOptions) error
	)

	// User must provide specify a host
	if pd.Host.Null {
		host = os.Getenv("THARSIS_ENDPOINT")
	} else {
		host = pd.Host.Value

		// Prepend scheme if only a hostname is passed in.
		_, err := url.ParseRequestURI(host)
		if err != nil {
			host = scheme + host
		}
	}

	if host == "" {
		return nil, fmt.Errorf("Host cannot be an empty string")
	}
	optFn = append(optFn, config.WithEndpoint(host))

	// Add TF_TOKEN_<host> value as first optFn as it is lowest priority
	if token := getTFTokenForHost(host); token != "" {
		tokenProvider, err := auth.NewStaticTokenProvider(token)
		if err != nil {
			return nil, fmt.Errorf("Failed to obtain a token provider for host \"%s\" using \"TF_TOKEN_\" environment variable: %v", host, err)
		}
		optFn = append(optFn, config.WithTokenProvider(tokenProvider))
	}

	if pd.StaticToken.Null {
		staticToken = os.Getenv("THARSIS_STATIC_TOKEN")
	} else {
		staticToken = pd.StaticToken.Value
	}

	if staticToken != "" {
		tokenProvider, err := auth.NewStaticTokenProvider(staticToken)
		if err != nil {
			return nil, fmt.Errorf("Failed to obtain a token provider for static token: %v", err)
		}
		optFn = append(optFn, config.WithTokenProvider(tokenProvider))
	}

	if pd.ServiceAccountPath.Null {
		serviceAccountPath = os.Getenv("THARSIS_SERVICE_ACCOUNT_PATH")
	} else {
		serviceAccountPath = pd.ServiceAccountPath.Value
	}

	if pd.ServiceAccountToken.Null {
		serviceAccountToken = os.Getenv("THARSIS_SERVICE_ACCOUNT_TOKEN")
	} else {
		serviceAccountToken = pd.ServiceAccountToken.Value
	}

	if (serviceAccountPath != "") && (serviceAccountToken != "") {
		tokenProvider, err := auth.NewServiceAccountTokenProvider(host, serviceAccountPath, serviceAccountToken)
		if err != nil {
			return nil, fmt.Errorf("Failed to obtain a token provider for service account %s: %v", serviceAccountPath, err)
		}
		optFn = append(optFn, config.WithTokenProvider(tokenProvider))
	}

	sdkConfig, err := config.Load(optFn...)
	if err != nil {
		return nil, err
	}

	return tharsis.NewClient(sdkConfig)
}

// convertProviderType is a helper function for NewResource and NewDataSource
// implementations to associate the concrete provider type. Alternatively,
// this helper can be skipped and the provider type can be directly type
// asserted (e.g. provider: in.(*provider)), however using this can prevent
// potential panics.
func convertProviderType(in provider.Provider) (tharsisProvider, diag.Diagnostics) {
	var diags diag.Diagnostics

	p, ok := in.(*tharsisProvider)

	if !ok {
		diags.AddError(
			"Unexpected Provider Instance Type",
			fmt.Sprintf("While creating the data source or resource, an unexpected provider type (%T) was received. This is always a bug in the provider code and should be reported to the provider developers.", p),
		)
		return tharsisProvider{}, diags
	}

	if p == nil {
		diags.AddError(
			"Unexpected Provider Instance Type",
			"While creating the data source or resource, an unexpected empty provider instance was received. This is always a bug in the provider code and should be reported to the provider developers.",
		)
		return tharsisProvider{}, diags
	}

	return *p, diags
}

func getTFTokenForHost(host string) string {
	if host == "" {
		// undefined host doesn't have a token
		return ""
	}

	uri, err := url.Parse(host)
	if err != nil {
		// can't provide a token if host can't be parsed
		return ""
	}

	hostname, err := svchost.ForComparison(uri.Host)
	if err != nil {
		// return an empty string if we can't compare
		return ""
	}

	return tfTokenEnvironmentVariables()[hostname]
}

// tfTokenEnvironmentVariables returns a map of valid hostnames and their token based on the `TF_TOKEN_` prefixed environment variables.
// This was copied from github.com/hashicorp/terraform-provider-tfe/tfe/credentials.go:collectCredentialsFromEnv with a license of MPL-2.0
func tfTokenEnvironmentVariables() map[svchost.Hostname]string {
	const prefix = "TF_TOKEN_"

	ret := make(map[svchost.Hostname]string)
	for _, ev := range os.Environ() {
		eqIdx := strings.Index(ev, "=")
		if eqIdx < 0 {
			continue
		}
		name := ev[:eqIdx]
		value := ev[eqIdx+1:]
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rawHost := name[len(prefix):]

		// We accept double underscores in place of hyphens because hyphens are not valid
		// identifiers in most shells and are therefore hard to set.
		// This is unambiguous with replacing single underscores below because
		// hyphens are not allowed at the beginning or end of a label and therefore
		// odd numbers of underscores will not appear together in a valid variable name.
		rawHost = strings.ReplaceAll(rawHost, "__", "-")

		// We accept underscores in place of dots because dots are not valid
		// identifiers in most shells and are therefore hard to set.
		// Underscores are not valid in hostnames, so this is unambiguous for
		// valid hostnames.
		rawHost = strings.ReplaceAll(rawHost, "_", ".")

		// Because environment variables are often set indirectly by OS
		// libraries that might interfere with how they are encoded, we'll
		// be tolerant of them being given either directly as UTF-8 IDNs
		// or in Punycode form, normalizing to Punycode form here because
		// that is what the Terraform credentials helper protocol will
		// use in its requests.
		//
		// Using ForDisplay first here makes this more liberal than Terraform
		// itself would usually be in that it will tolerate pre-punycoded
		// hostnames that Terraform normally rejects in other contexts in order
		// to ensure stored hostnames are human-readable.
		dispHost := svchost.ForDisplay(rawHost)
		hostname, err := svchost.ForComparison(dispHost)
		if err != nil {
			// Ignore invalid hostnames
			continue
		}

		ret[hostname] = value
	}

	return ret
}
