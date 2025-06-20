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

package bao

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"emperror.dev/errors"
	baoapi "github.com/hashicorp/vault/api"
	"github.com/spf13/cast"

	"github.com/bank-vaults/vault-sdk/utils/templater"
	bao "github.com/bank-vaults/vault-sdk/vault"
)

type SecretInjectorFunc func(key, value string)

type SecretRenewer interface {
	Renew(path string, secret *baoapi.Secret) error
}

type Config struct {
	TransitKeyID         string
	TransitPath          string
	TransitBatchSize     int
	IgnoreMissingSecrets bool
	DaemonMode           bool
}

type SecretInjector struct {
	mu           sync.RWMutex
	config       Config
	client       *bao.Client
	renewer      SecretRenewer
	logger       *slog.Logger
	transitCache map[string][]byte
	secretCache  map[string]map[string]interface{}
}

func NewSecretInjector(config Config, client *bao.Client, renewer SecretRenewer, logger *slog.Logger) SecretInjector {
	return SecretInjector{
		config:       config,
		client:       client,
		renewer:      renewer,
		logger:       logger,
		transitCache: map[string][]byte{},
		secretCache:  map[string]map[string]interface{}{},
	}
}

var inlineMutationRegex = regexp.MustCompile(`\${([>]{0,2}bao:.*?#*}?)}`)

func (i *SecretInjector) FetchTransitSecrets(secrets []string) (map[string][]byte, error) {
	return i.FetchTransitSecretsWithContext(context.Background(), secrets)
}

func (i *SecretInjector) FetchTransitSecretsWithContext(ctx context.Context, secrets []string) (map[string][]byte, error) {
	if len(i.config.TransitKeyID) == 0 {
		return map[string][]byte{}, errors.Errorf("found encrypted variable, but transit key ID is empty: %s", "todo")
	}

	if len(secrets) == 0 {
		return map[string][]byte{}, nil
	}

	out, err := i.client.Transit.DecryptBatchWithContext(ctx, i.config.TransitPath, i.config.TransitKeyID, secrets)
	if err != nil {
		i.logger.Error(fmt.Sprintf("failed to decrypt variable: %s", err))
	}

	i.mu.Lock()
	for k, v := range out {
		i.transitCache[k] = v
	}
	i.mu.Unlock()

	return out, nil
}

func paginate(secrets []string, batchSize int) [][]string {
	transitSecrets := [][]string{}

	for i := range secrets {
		if i%batchSize == 0 {
			transitSecrets = append(transitSecrets, []string{})
		}

		index := i / batchSize

		transitSecrets[index] = append(transitSecrets[index], secrets[i])
	}

	return transitSecrets
}

func (i *SecretInjector) preprocessTransitSecrets(references *map[string]string, inject SecretInjectorFunc) error {
	// use set so that we don't have duplicates
	secretSet := map[string]bool{}

	for _, value := range *references {
		// decrypts value with Bao Transit Secret Engine
		if HasInlineBaoDelimiters(value) {
			for _, baoSecretReference := range FindInlineBaoDelimiters(value) {
				if i.client.Transit.IsEncrypted(baoSecretReference[1]) {
					secretSet[baoSecretReference[1]] = true
				}
			}
		} else if i.client.Transit.IsEncrypted(value) {
			secretSet[value] = true
		}
	}

	// convert back to slice & filter out already-cached secrets
	secrets := make([]string, 0, len(secretSet))
	i.mu.RLock()
	for k := range secretSet {
		if _, cached := i.transitCache[k]; !cached {
			secrets = append(secrets, k)
		}
	}
	i.mu.RUnlock()

	for _, sec := range paginate(secrets, i.config.TransitBatchSize) {
		_, err := i.FetchTransitSecrets(sec)
		if err != nil {
			if !i.config.IgnoreMissingSecrets {
				return errors.Wrapf(err, "failed to decrypt secret: %s", sec)
			}

			i.logger.Error(fmt.Sprintf("failed to decrypt secret: %s", err), slog.Any("secrets", sec))
		}
	}

	for name, value := range *references {
		if HasInlineBaoDelimiters(value) {
			newValue := value
			i.mu.RLock()
			for _, baoSecretReference := range FindInlineBaoDelimiters(value) {
				if v, ok := i.transitCache[baoSecretReference[0]]; ok {
					newValue = strings.ReplaceAll(value, baoSecretReference[0], string(v))
				}
			}
			i.mu.RUnlock()

			// Only inject the value if its content has been updated using the transit cache
			if value != newValue {
				inject(name, value)

				// Delete the key from the references to avoid a double processing by the old logic
				delete(*references, name)
			}

			continue
		}
		if i.client.Transit.IsEncrypted(value) {
			i.mu.RLock()
			v, ok := i.transitCache[value]
			i.mu.RUnlock()
			if ok {
				inject(name, string(v))

				continue
			}
		}
	}

	return nil
}

func (i *SecretInjector) InjectSecretsFromBao(references map[string]string, inject SecretInjectorFunc) error {
	return i.InjectSecretsFromBaoWithContext(context.Background(), references, inject)
}

func (i *SecretInjector) InjectSecretsFromBaoWithContext(ctx context.Context, references map[string]string, inject SecretInjectorFunc) error {
	err := i.preprocessTransitSecrets(&references, inject)
	if err != nil && !i.config.IgnoreMissingSecrets {
		return errors.Wrapf(err, "unable to preprocess transit secrets")
	}

	for name, value := range references {
		if HasInlineBaoDelimiters(value) {
			for _, baoSecretReference := range FindInlineBaoDelimiters(value) {
				mapData, err := i.GetDataFromBaoWithContext(ctx, map[string]string{name: baoSecretReference[1]})
				if err != nil {
					return err
				}
				for _, v := range mapData {
					value = strings.ReplaceAll(value, baoSecretReference[0], v)
				}
			}
			inject(name, value)

			continue
		}

		var update bool
		if strings.HasPrefix(value, ">>bao:") {
			value = strings.TrimPrefix(value, ">>")
			update = true
		} else {
			update = false
		}

		if !strings.HasPrefix(value, "bao:") {
			inject(name, value)

			continue
		}

		valuePath := strings.TrimPrefix(value, "bao:")

		// handle special case for bao:login env value
		// namely pass through the BAO_TOKEN received from the Bao login procedure
		if name == "BAO_TOKEN" && valuePath == "login" {
			value = i.client.RawClient().Token()
			inject(name, value)

			continue
		}

		// decrypts value with Bao Transit Secret Engine
		if i.client.Transit.IsEncrypted(value) {
			if len(i.config.TransitKeyID) == 0 {
				return errors.Errorf("found encrypted variable, but transit key ID is empty: %s", name)
			}

			i.mu.RLock()
			v, ok := i.transitCache[value]
			i.mu.RUnlock()
			if ok {
				inject(name, string(v))

				continue
			}

			out, err := i.client.Transit.DecryptWithContext(ctx, i.config.TransitPath, i.config.TransitKeyID, []byte(value))
			if err != nil {
				if !i.config.IgnoreMissingSecrets {
					return errors.Wrapf(err, "failed to decrypt variable: %s", name)
				}

				i.logger.Error(fmt.Sprintf("failed to decrypt variable: %s", err), slog.String("variable", name))

				continue
			}

			i.mu.Lock()
			i.transitCache[value] = out
			i.mu.Unlock()

			inject(name, string(out))

			continue
		}

		split := strings.SplitN(valuePath, "#", 3)
		valuePath = split[0]

		if len(split) < 2 {
			return errors.New("secret data key or template not defined")
		}

		key := split[1]

		versionOrData := "-1"
		if update {
			versionOrData = "{}"
		}
		if len(split) == 3 {
			versionOrData = split[2]
		}

		secretCacheKey := valuePath + "#" + versionOrData
		var data map[string]interface{}
		var err error

		i.mu.RLock()
		if data = i.secretCache[secretCacheKey]; data == nil {
			data, err = i.readBaoPath(ctx, valuePath, versionOrData, update)
		}
		i.mu.RUnlock()

		if err != nil {
			return err
		}

		if data == nil {
			if !i.config.IgnoreMissingSecrets {
				return errors.Errorf("path not found: %s", valuePath)
			}
			i.logger.Warn(fmt.Sprintf("path not found %s", valuePath))

			continue
		}

		i.mu.Lock()
		i.secretCache[secretCacheKey] = data
		i.mu.Unlock()

		templater := templater.NewTemplater(templater.DefaultLeftDelimiter, templater.DefaultRightDelimiter)

		if templater.IsGoTemplate(key) {
			value, err := templater.Template(key, data)
			if err != nil {
				return errors.Wrapf(err, "failed to interpolate template key with bao data: %s", key)
			}
			inject(name, value.String())
		} else {
			if value, ok := data[key]; ok {
				value, err := cast.ToStringE(value)
				if err != nil {
					return errors.Wrap(err, "value can't be cast to a string")
				}
				inject(name, value)
			} else {
				return errors.Errorf("key '%s' not found under path: %s", key, valuePath)
			}
		}
	}

	return nil
}

func (i *SecretInjector) InjectSecretsFromBaoPath(paths string, inject SecretInjectorFunc) error {
	return i.InjectSecretsFromBaoPathWithContext(context.Background(), paths, inject)
}

func (i *SecretInjector) InjectSecretsFromBaoPathWithContext(ctx context.Context, paths string, inject SecretInjectorFunc) error {
	baoPaths := strings.Split(paths, ",")

	for _, path := range baoPaths {
		split := strings.SplitN(path, "#", 2)
		valuePath := split[0]

		version := "-1"

		if len(split) == 2 {
			version = split[1]
		}

		data, err := i.readBaoPath(ctx, valuePath, version, false)
		if err != nil {
			return err
		}

		if data == nil {
			if !i.config.IgnoreMissingSecrets {
				return errors.Errorf("path not found: %s", valuePath)
			}
			i.logger.Warn(fmt.Sprintf("path not found %s", valuePath))

			continue
		}

		for key, value := range data {
			value, err := cast.ToStringE(value)
			if err != nil {
				return errors.Wrap(err, "value can't be cast to a string for key: "+key)
			}
			inject(key, value)
		}
	}

	return nil
}

func (i *SecretInjector) readBaoPath(ctx context.Context, path, versionOrData string, update bool) (map[string]interface{}, error) {
	var secretData map[string]interface{}

	var secret *baoapi.Secret
	var err error

	if update {
		var data map[string]interface{}
		err = json.Unmarshal([]byte(versionOrData), &data)
		if err != nil {
			return nil, errors.Wrap(err, "failed to unmarshal data for writing")
		}

		secret, err = i.client.RawClient().Logical().WriteWithContext(ctx, path, data)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to write secret to path: %s", path)
		}
	} else {
		secret, err = i.client.RawClient().Logical().ReadWithDataWithContext(ctx, path, map[string][]string{"version": {versionOrData}})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read secret from path: %s", path)
		}
	}

	if i.config.DaemonMode && secret != nil && secret.LeaseDuration > 0 {
		i.logger.Info("secret has a lease duration, starting renewal", slog.String("path", path), slog.Int("lease-duration", secret.LeaseDuration))

		err = i.renewer.Renew(path, secret)
		if err != nil {
			return nil, errors.Wrap(err, "secret renewal can't be established")
		}
	}

	if secret == nil {
		return nil, nil
	}

	for _, warning := range secret.Warnings {
		i.logger.Warn(warning, slog.String("path", path))
	}

	v2Data, ok := secret.Data["data"]
	if ok {
		secretData = cast.ToStringMap(v2Data)

		// Handle the case where "metadata" key is not present or is nil.
		metadataRaw, ok := secret.Data["metadata"]
		if metadataRaw == nil || !ok {
			return nil, errors.New("metadata key not found or is nil in secret")
		}

		// Handle the case where the type assertion fails.
		metadata, ok := metadataRaw.(map[string]interface{})
		if !ok {
			return nil, errors.New("metadata has an unexpected type")
		}

		// Check if a given version of a path is destroyed
		// Handle the case where "destroyed" key is not present or has an unexpected type.
		destroyed, _ := metadata["destroyed"].(bool)
		if destroyed {
			i.logger.Warn("version of secret has been permanently destroyed", slog.String("path", path), slog.String("version", versionOrData))
		}

		// Check if a given version of a path still exists
		if deletionTime, ok := metadata["deletion_time"].(string); ok && deletionTime != "" {
			i.logger.Warn(
				"cannot find data for path, given version has been deleted",
				slog.String("path", path),
				slog.String("version", versionOrData),
				slog.String("deletion-time", deletionTime),
			)
		}
	} else {
		secretData = cast.ToStringMap(secret.Data)
	}

	return secretData, nil
}

func IsValidPrefix(value string) bool {
	return strings.HasPrefix(value, "bao:") || strings.HasPrefix(value, ">>bao:")
}

func HasInlineBaoDelimiters(value string) bool {
	return len(FindInlineBaoDelimiters(value)) > 0
}

func FindInlineBaoDelimiters(value string) [][]string {
	return inlineMutationRegex.FindAllStringSubmatch(value, -1)
}

func (i *SecretInjector) GetDataFromBao(data map[string]string) (map[string]string, error) {
	return i.GetDataFromBaoWithContext(context.Background(), data)
}

func (i *SecretInjector) GetDataFromBaoWithContext(ctx context.Context, data map[string]string) (map[string]string, error) {
	baoData := make(map[string]string, len(data))

	inject := func(key, value string) {
		baoData[key] = value
	}

	return baoData, i.InjectSecretsFromBao(data, inject)
}
