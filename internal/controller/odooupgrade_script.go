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

// Script generation for an install that also sets the admin password. Pure functions, so
// the script can be unit tested and checked with `sh -n`.

import (
	"regexp"
	"strings"
)

// upgradeAdminPasswordSnippet is read by `odoo shell` on stdin. It takes the password from
// the environment, so the value never appears in the script or in the command line.
const upgradeAdminPasswordSnippet = `import os
u = env.ref('base.user_admin')
u.password = os.environ['ODOO_ADMIN_USER_PASSWORD']
env.cr.commit()
`

// shSafeWord matches arguments that need no quoting in a POSIX shell.
var shSafeWord = regexp.MustCompile(`^[a-zA-Z0-9_./=,:+-]+$`)

// shQuoteWord quotes one shell word. Safe words are returned unchanged.
func shQuoteWord(s string) string {
	if shSafeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shJoin quotes each word and joins them with single spaces.
func shJoin(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shQuoteWord(w)
	}
	return strings.Join(quoted, " ")
}

// upgradeInitScript returns the shell script of an install that also sets the admin
// password. The first command is the install, with the same arguments the image entrypoint
// would get. The second runs odoo shell with the snippet on stdin. `set -e` fails the Job
// when either step fails. The password is never part of the script: the snippet reads it
// from ODOO_ADMIN_USER_PASSWORD.
func upgradeInitScript(database string, initArgs []string) string {
	install := append([]string{"odoo"}, initArgs...)
	shell := []string{
		"odoo", upgradeSubcmdShell,
		upgradeFlagConfig, odooConfPath,
		upgradeFlagDB, database,
		upgradeFlagNoHTTP,
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(shJoin(install))
	b.WriteString("\n")
	b.WriteString(shJoin(shell))
	b.WriteString(" <<'EOF'\n")
	b.WriteString(upgradeAdminPasswordSnippet)
	b.WriteString("EOF\n")
	return b.String()
}
