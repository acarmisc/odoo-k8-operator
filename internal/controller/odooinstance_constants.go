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

// Names used by the OdooInstance reconciler. Shared label keys and volume
// names live in constants.go.
const (
	labelAppName      = "app.kubernetes.io/name"
	labelAppInstance  = "app.kubernetes.io/instance"
	labelManagedBy    = "app.kubernetes.io/managed-by"
	managedByOperator = "odoo-k8-operator"

	annotationConfigHash  = "odoo.operator.io/config-hash"
	annotationSecretsHash = "odoo.operator.io/secrets-hash"
	annotationAddonsHash  = "odoo.operator.io/addons-hash"

	secretKeyPassword = "password"

	envPGPassword  = "PGPASSWORD"
	envDBPassword  = "DB_PASSWORD"
	envPGSSLMode   = "PGSSLMODE"
	envAdminPasswd = "ODOO_ADMIN_PASSWD"
	sslModeRequire = "require"

	portOdoo            int32 = 8069
	portLongpolling     int32 = 8072
	portNameOdoo              = "odoo"
	portNameLongpolling       = "longpolling"

	healthPath = "/web/health"

	volumeTmp       = "tmp"
	mountPathTmp    = "/tmp"
	mountPathConfig = "/etc/odoo"
	mountPathData   = "/var/lib/odoo"
	confFileName    = "odoo.conf"
	odooConfPath    = mountPathConfig + "/" + confFileName

	confKeyAddonsPath = "addons_path"
	confKeyProxyMode  = "proxy_mode"
	confKeyListDB     = "list_db"
	valueTrue         = "True"
	valueFalse        = "False"

	suffixConfigMap   = "-config"
	suffixAddonsPVC   = "-addons"
	suffixDataPVC     = "-data"
	defaultAddonsSize = "10Gi"
	defaultDataSize   = "5Gi"

	odooUID              int64 = 100
	terminationGraceSecs int64 = 60
)
