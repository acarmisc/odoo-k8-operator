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
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// rolloutHashes are stamped on the pod template. A change to any of them
// changes the template and therefore rolls the pods.
type rolloutHashes struct {
	config  string
	secrets string
	addons  string
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// configHash fingerprints the rendered odoo.conf.
func configHash(conf string) string {
	return sha256Hex(conf)
}

// secretsHash fingerprints the data of the given secrets. Keys are visited in
// sorted order and the secrets in the order given, so the result does not
// depend on map iteration order.
func secretsHash(secrets []*corev1.Secret) string {
	var b strings.Builder
	for _, secret := range secrets {
		for _, key := range slices.Sorted(maps.Keys(secret.Data)) {
			b.WriteString(key + "=" + hex.EncodeToString(secret.Data[key]) + "\n")
		}
		b.WriteString("---\n")
	}
	return sha256Hex(b.String())
}
