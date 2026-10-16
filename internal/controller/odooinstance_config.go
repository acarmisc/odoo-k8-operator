/*
Copyright 2026 Odoo K8s Operator.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// blockedConfigKeys are options the operator owns; spec.config may not set them.
var blockedConfigKeys = map[string]bool{
	"admin_passwd": true,
	"db_password":  true,
	"dbfilter":     true,
}

// odooConfDefaults returns production defaults that spec.config may override.
func odooConfDefaults() map[string]string {
	return map[string]string{
		confKeyAddonsPath: addonsMountPath,
		confKeyProxyMode:  valueTrue,
		confKeyListDB:     valueFalse,
	}
}

type confPair struct {
	key   string
	value string
}

// renderOdooConf produces the contents of odoo.conf. Every key, defaults and
// user-supplied alike, is emitted in sorted order so the output (and the
// config hash derived from it) is deterministic.
func renderOdooConf(spec *odoov1.OdooInstanceSpec) (string, error) {
	merged := odooConfDefaults()

	for _, key := range slices.Sorted(maps.Keys(spec.Config)) {
		value := spec.Config[key]
		if blockedConfigKeys[key] {
			return "", fmt.Errorf("config key %q is not permitted in spec.config", key)
		}
		if key == "" || strings.ContainsAny(key, "=[]\n\r") {
			return "", fmt.Errorf("config key %q contains illegal characters", key)
		}
		if err := checkConfValue(key, value); err != nil {
			return "", err
		}
		merged[key] = value
	}

	if spec.Postgres.Database != "" {
		dbSettings := []confPair{
			{"db_host", spec.Postgres.Host},
			{"db_port", strconv.Itoa(int(spec.Postgres.Port))},
			{"db_name", spec.Postgres.Database},
			{"db_user", spec.Postgres.User},
			{"db_sslmode", sslModeRequire},
		}
		for _, pair := range dbSettings {
			if err := checkConfValue(pair.key, pair.value); err != nil {
				return "", err
			}
			merged[pair.key] = pair.value
		}
	}

	var b strings.Builder
	b.WriteString("[options]\n")
	for _, key := range slices.Sorted(maps.Keys(merged)) {
		b.WriteString(key + " = " + merged[key] + "\n")
	}
	return b.String(), nil
}

// checkConfValue rejects values that would break the one-line-per-key format.
func checkConfValue(key, value string) error {
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("config value for key %q contains illegal newline characters", key)
	}
	return nil
}
