// Copyright © 2020 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vault

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"emperror.dev/errors"
	"github.com/fsnotify/fsnotify"
	vaultapi "github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/api/auth/aws"
	"github.com/hashicorp/vault/api/auth/azure"
	"github.com/hashicorp/vault/api/auth/gcp"
	"github.com/hashicorp/vault/api/auth/kubernetes"
)

const (
	defaultJWTFile       = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	sessionCacheCapacity = 64
)

// NewData is a helper function for Vault KV Version two secret data creation
func NewData(cas int, data map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"options": map[string]interface{}{"cas": cas},
		"data":    data,
	}
}

type clientOptions struct {
	url            string
	role           string
	authPath       string
	tokenPath      string
	token          string
	timeout        time.Duration
	logger         Logger
	authMethod     ClientAuthMethod
	existingSecret string
	vaultNamespace string
}

// ClientOption configures a Vault client using the functional options paradigm popularized by Rob Pike and Dave Cheney.
// If you're unfamiliar with this style,
// see https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html and
// https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis.
type ClientOption interface {
	apply(o *clientOptions)
}

// ClientURL is the vault url EX: https://my-vault.vault.org
type ClientURL string

func (co ClientURL) apply(o *clientOptions) {
	o.url = string(co)
}

// ClientRole is the vault role which the client would like to receive
type ClientRole string

func (co ClientRole) apply(o *clientOptions) {
	o.role = string(co)
}

// ClientAuthPath is the mount path where the auth method is enabled.
type ClientAuthPath string

func (co ClientAuthPath) apply(o *clientOptions) {
	o.authPath = string(co)
}

// ClientTokenPath file where the Vault token can be found.
type ClientTokenPath string

func (co ClientTokenPath) apply(o *clientOptions) {
	o.tokenPath = string(co)
}

// ClientToken is a Vault token.
type ClientToken string

func (co ClientToken) apply(o *clientOptions) {
	o.token = string(co)
}

// ClientTimeout after which the client fails.
type ClientTimeout time.Duration

func (co ClientTimeout) apply(o *clientOptions) {
	o.timeout = time.Duration(co)
}

// ClientLogger wraps a logur.Logger compatible logger to be used in the client.
func ClientLogger(logger Logger) clientLogger {
	return clientLogger{logger: logger}
}

type clientLogger struct {
	logger Logger
}

func (co clientLogger) apply(o *clientOptions) {
	o.logger = co.logger
}

// ClientAuthMethod file where the Vault token can be found.
type ClientAuthMethod string

func (co ClientAuthMethod) apply(o *clientOptions) {
	o.authMethod = co
}

type ExistingSecret string

func (co ExistingSecret) apply(o *clientOptions) {
	o.existingSecret = string(co)
}

// Vault Enterprise Namespace (not Kubernetes namespace)
type VaultNamespace string

func (co VaultNamespace) apply(o *clientOptions) {
	o.vaultNamespace = string(co)
}

const (
	// AWSEC2AuthMethod is used for the Vault AWS EC2 auth method
	// as described here: https://www.vaultproject.io/docs/auth/aws#ec2-auth-method
	AWSEC2AuthMethod ClientAuthMethod = "aws-ec2"

	// AWSIAMAuthMethod is used for the Vault AWS IAM auth method
	// as described here: https://www.vaultproject.io/docs/auth/aws#iam-auth-method
	AWSIAMAuthMethod ClientAuthMethod = "aws-iam"

	// GCPGCEAuthMethod is used for the Vault GCP GCE auth method
	// as described here: https://www.vaultproject.io/docs/auth/gcp#gce-login
	GCPGCEAuthMethod ClientAuthMethod = "gcp-gce"

	// GCPIAMAuthMethod is used for the Vault GCP IAM auth method
	// as described here: https://www.vaultproject.io/docs/auth/gcp#iam
	GCPIAMAuthMethod ClientAuthMethod = "gcp-iam"

	// JWTAuthMethod is used for the Vault JWT/OIDC/GCP/Kubernetes auth methods
	// as describe here:
	// - https://www.vaultproject.io/docs/auth/jwt
	// - https://www.vaultproject.io/docs/auth/kubernetes
	// - https://www.vaultproject.io/docs/auth/gcp
	JWTAuthMethod ClientAuthMethod = "jwt"

	// AzureMSIAuthMethod is used for the vault Azure auth method
	// as described here:
	// - https://www.vaultproject.io/docs/auth/azure
	AzureMSIAuthMethod ClientAuthMethod = "azure"

	// NamespacedSecretAuthMethod is used for per namespace secrets
	NamespacedSecretAuthMethod ClientAuthMethod = "namespaced"
)

// Client is a Vault client with Kubernetes support, token automatic renewing and
// access to Transit Secret Engine wrapper
type Client struct {
	// Easy to use wrapper for transit secret engine calls
	Transit *Transit

	client       *vaultapi.Client
	logical      *vaultapi.Logical
	tokenWatcher *vaultapi.Renewer
	closed       bool
	watch        *fsnotify.Watcher
	mu           sync.Mutex
	logger       Logger
}

// NewClient creates a new Vault client.
func NewClient(role string) (*Client, error) {
	return NewClientWithOptions(ClientRole(role))
}

// NewClientWithOptions creates a new Vault client with custom options.
func NewClientWithOptions(opts ...ClientOption) (*Client, error) {
	config := vaultapi.DefaultConfig()
	if config.Error != nil {
		return nil, config.Error
	}
	return NewClientFromConfig(config, opts...)
}

// NewClientWithConfig creates a new Vault client with custom configuration.
// Deprecated: use NewClientFromConfig instead.
func NewClientWithConfig(config *vaultapi.Config, role, path string) (*Client, error) {
	return NewClientFromConfig(config, ClientRole(role), ClientAuthPath(path))
}

// NewClientFromConfig creates a new Vault client from custom configuration.
func NewClientFromConfig(config *vaultapi.Config, opts ...ClientOption) (*Client, error) {
	return NewClientFromConfigWithContext(context.Background(), config, opts...)
}

// NewClientFromConfig creates a new Vault client from custom configuration with context.
func NewClientFromConfigWithContext(ctx context.Context, config *vaultapi.Config, opts ...ClientOption) (*Client, error) {
	rawClient, err := vaultapi.NewClient(config)
	if err != nil {
		return nil, err
	}

	client, err := NewClientFromRawClientWithContext(ctx, rawClient, opts...)
	if err != nil {
		return nil, err
	}

	caCertPath := os.Getenv(vaultapi.EnvVaultCACert)
	caCertReload := os.Getenv("VAULT_CACERT_RELOAD") != "false"

	if caCertPath != "" && caCertReload {
		watch, err := fsnotify.NewWatcher()
		if err != nil {
			return nil, err
		}

		caCertFile := filepath.Clean(caCertPath)
		configDir, _ := filepath.Split(caCertFile)

		_ = watch.Add(configDir)

		go func() {
			for {
				client.mu.Lock()
				if client.closed {
					client.mu.Unlock()
					break
				}
				client.mu.Unlock()

				select {
				case event := <-watch.Events:
					// we only care about the CA cert file or the Secret mount directory (if in Kubernetes)
					if filepath.Clean(event.Name) == caCertFile || filepath.Base(event.Name) == "..data" {
						if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
							err := config.ReadEnvironment()
							if err != nil {
								client.logger.Error("failed to reload Vault config", map[string]interface{}{"err": err})
							} else {
								client.logger.Info("CA certificate reloaded")
							}
						}
					}
				case err := <-watch.Errors:
					client.logger.Error("watcher error", map[string]interface{}{"err": err})
				}
			}
		}()

		client.watch = watch
	}

	return client, nil
}

// NewClientFromRawClient creates a new Vault client from custom raw client.
func NewClientFromRawClient(rawClient *vaultapi.Client, opts ...ClientOption) (*Client, error) {
	return NewClientFromRawClientWithContext(context.Background(), rawClient, opts...)
}

// NewClientFromRawClientWithContext creates a new Vault client from custom raw client.
func NewClientFromRawClientWithContext(ctx context.Context, rawClient *vaultapi.Client, opts ...ClientOption) (*Client, error) {
	logical := rawClient.Logical()
	transit := &Transit{
		client: rawClient,
	}
	client := &Client{
		Transit: transit,
		client:  rawClient,
		logical: logical,
		logger:  noopLogger{},
	}

	var tokenWatcher *vaultapi.Renewer

	o := &clientOptions{}

	for _, opt := range opts {
		opt.apply(o)
	}

	// Set logger
	if o.logger != nil {
		client.logger = o.logger
	}

	// Set URL if defined
	if o.url != "" {
		err := rawClient.SetAddress(o.url)
		if err != nil {
			return nil, err
		}
	}

	// Default role
	if o.role == "" {
		o.role = "default"
	}

	// Default auth path
	if o.authPath == "" {
		o.authPath = "kubernetes"
	}

	if o.authMethod == "" {
		o.authMethod = JWTAuthMethod
	}

	// Default token path
	if o.tokenPath == "" {
		o.tokenPath = os.Getenv("HOME") + "/.vault-token"
		if env, ok := os.LookupEnv("VAULT_TOKEN_PATH"); ok {
			o.tokenPath = env
		}
	}

	// Set vault namespace if defined
	if o.vaultNamespace != "" {
		rawClient.SetNamespace(o.vaultNamespace)
	}

	// Default timeout
	if o.timeout == 0 {
		o.timeout = 10 * time.Second
		if env, ok := os.LookupEnv("VAULT_CLIENT_TIMEOUT"); ok {
			var err error
			if o.timeout, err = time.ParseDuration(env); err != nil {
				return nil, errors.Wrap(err, "could not parse timeout duration")
			}
		}
	}

	// Add token if set
	if o.token != "" {
		rawClient.SetToken(o.token)
	} else if rawClient.Token() == "" {
		token, err := os.ReadFile(o.tokenPath)
		if err == nil {
			rawClient.SetToken(string(token))
		} else {
			// If VAULT_TOKEN, VAULT_TOKEN_PATH or ~/.vault-token wasn't provided,
			// attempt to get one with supported JWT-based authentication methods
			// (such as Kubernetes ServiceAccount JWT).

			jwtFile := defaultJWTFile
			if file := os.Getenv("KUBERNETES_SERVICE_ACCOUNT_TOKEN"); file != "" {
				jwtFile = file
			} else if file := os.Getenv("VAULT_JWT_FILE"); file != "" {
				jwtFile = file
			}

			initialTokenArrived := make(chan string, 1)
			initialTokenSent := false

			go func() {
				for {
					client.mu.Lock()
					if client.closed {
						client.mu.Unlock()
						break
					}
					client.mu.Unlock()

					secret, err := client.getVaultAPISecret(ctx, jwtFile, o)
					if err != nil {
						client.logger.Error("failed to request new Vault token", map[string]interface{}{"err": err})
						time.Sleep(1 * time.Second)
						continue
					}

					if secret == nil {
						client.logger.Debug("received empty answer from Vault, retrying")
						time.Sleep(1 * time.Second)
						continue
					}

					client.logger.Info("received new Vault token", map[string]interface{}{
						"addr": o.url,
						"role": o.role,
						"path": o.authPath,
					})

					// Set the first token from the response
					rawClient.SetToken(secret.Auth.ClientToken)

					if !initialTokenSent {
						initialTokenArrived <- secret.LeaseID
						initialTokenSent = true
					}

					// Start the renewing process
					tokenWatcher, err = rawClient.NewLifetimeWatcher(&vaultapi.LifetimeWatcherInput{Secret: secret})
					if err != nil {
						client.logger.Error("failed to watch Vault token", map[string]interface{}{"err": err})
						continue
					}

					client.mu.Lock()
					client.tokenWatcher = tokenWatcher
					client.mu.Unlock()

					go tokenWatcher.Start()

					client.runRenewChecker(tokenWatcher)
				}

				client.logger.Info("Vault token renewal closed")
			}()

			select {
			case <-initialTokenArrived:
				client.logger.Info("initial Vault token arrived")

			case <-time.After(o.timeout):
				client.Close()
				return nil, errors.Errorf("timeout [%s] during waiting for Vault token", o.timeout)
			}
		}
	}

	return client, nil
}

func (client *Client) getVaultAPISecret(ctx context.Context, jwtFile string, o *clientOptions) (*vaultapi.Secret, error) {
	switch o.authMethod { //nolint:exhaustive
	case AWSEC2AuthMethod:
		jwt, err := os.ReadFile(jwtFile)
		if err != nil {
			return nil, err
		}
		nonce := fmt.Sprintf("%x", sha256.Sum256(jwt))

		awsAuth, err := aws.NewAWSAuth(aws.WithRole(o.role), aws.WithMountPath(o.authPath), aws.WithEC2Auth(), aws.WithPKCS7Signature(), aws.WithNonce(nonce))
		if err != nil {
			return nil, err
		}

		return awsAuth.Login(ctx, client.RawClient())

	case AWSIAMAuthMethod:
		awsAuth, err := aws.NewAWSAuth(aws.WithRole(o.role), aws.WithMountPath(o.authPath), aws.WithIAMAuth())
		if err != nil {
			return nil, err
		}

		return awsAuth.Login(ctx, client.RawClient())

	case GCPGCEAuthMethod:
		gcpAuth, err := gcp.NewGCPAuth(o.role, gcp.WithGCEAuth(), gcp.WithMountPath(o.authPath))
		if err != nil {
			return nil, err
		}
		return gcpAuth.Login(ctx, client.RawClient())

	case GCPIAMAuthMethod:
		serviceAccountEmail, err := metadata.EmailWithContext(ctx, "default")
		if err != nil {
			return nil, err
		}

		gcpAuth, err := gcp.NewGCPAuth(o.role, gcp.WithIAMAuth(serviceAccountEmail), gcp.WithMountPath(o.authPath))
		if err != nil {
			return nil, err
		}
		return gcpAuth.Login(ctx, client.RawClient())

	case AzureMSIAuthMethod:
		azureAuth, err := azure.NewAzureAuth(o.role, azure.WithMountPath(o.authPath))
		if err != nil {
			return nil, err
		}
		return azureAuth.Login(ctx, client.RawClient())

	case NamespacedSecretAuthMethod:
		if len(o.existingSecret) > 0 {
			kubernetesAuth, err := kubernetes.NewKubernetesAuth(o.role, kubernetes.WithServiceAccountToken(o.existingSecret), kubernetes.WithMountPath(o.authPath))
			if err != nil {
				return nil, err
			}
			return kubernetesAuth.Login(ctx, client.RawClient())
		}
		fallthrough

	// 'jwt' or 'kubernetes', ends up doing JWT as it also works for Kubernetes
	default:
		jwt, err := os.ReadFile(jwtFile)
		if err != nil {
			return nil, err
		}

		kubernetesAuth, err := kubernetes.NewKubernetesAuth(o.role, kubernetes.WithServiceAccountToken(string(jwt)), kubernetes.WithMountPath(o.authPath))
		if err != nil {
			return nil, err
		}
		return kubernetesAuth.Login(ctx, client.RawClient())
	}
}

func (client *Client) runRenewChecker(tokenWatcher *vaultapi.Renewer) {
	for {
		select {
		case err := <-tokenWatcher.DoneCh():
			if err != nil {
				client.logger.Error("error in Vault token renewal", map[string]interface{}{"err": err})
			}
			return
		case o := <-tokenWatcher.RenewCh():
			ttl, _ := o.Secret.TokenTTL()
			client.logger.Info("renewed Vault token", map[string]interface{}{"ttl": ttl})
		}
	}
}

// Vault returns the underlying hashicorp Vault client.
// Deprecated: use RawClient instead.
func (client *Client) Vault() *vaultapi.Client {
	return client.RawClient()
}

// RawClient returns the underlying raw Vault client.
func (client *Client) RawClient() *vaultapi.Client {
	return client.client
}

// Close stops the token renewing process of this client
func (client *Client) Close() {
	client.mu.Lock()
	defer client.mu.Unlock()

	client.closed = true

	if client.tokenWatcher != nil {
		client.tokenWatcher.Stop()
	}

	if client.watch != nil {
		_ = client.watch.Close()
	}
}

// NewRawClient creates a new raw Vault client.
func NewRawClient() (*vaultapi.Client, error) {
	config := vaultapi.DefaultConfig()
	if config.Error != nil {
		return nil, config.Error
	}

	config.HttpClient.Transport.(*http.Transport).TLSHandshakeTimeout = 5 * time.Second

	return vaultapi.NewClient(config)
}

// NewInsecureRawClient creates a new raw Vault client with insecure TLS.
func NewInsecureRawClient() (*vaultapi.Client, error) {
	config := vaultapi.DefaultConfig()
	if config.Error != nil {
		return nil, config.Error
	}

	config.HttpClient.Transport.(*http.Transport).TLSHandshakeTimeout = 5 * time.Second
	config.HttpClient.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
	config.HttpClient.Transport.(*http.Transport).TLSClientConfig.ClientSessionCache = tls.NewLRUClientSessionCache(sessionCacheCapacity)

	return vaultapi.NewClient(config)
}
